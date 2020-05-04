package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-ble/ble"
	"github.com/go-ble/ble/linux"
	"github.com/pkg/errors"
)

var dur *time.Duration
var dir *string
var port *int
var logger *log.Logger
var stop bool = true

// Beacon represents a BLE beacon
type Beacon struct {
	MAC              string    `json:"mac"`
	Detected         time.Time `json:"detected"`
	Name             string    `json:"name"`
	UUID             string    `json:"uuid"`
	Major            string    `json:"major"`
	Minor            string    `json:"minor"`
	RSSI             int       `json:"rssi"`
	ManufacturerData string    `json:"manufacturer_data"`
	ServiceUUID      string    `json:"service_uuid"`
	ServiceData      string    `json:"service_data"`
	Battery          string    `json:"battery"`
	Temperature      string    `json:"temperature"`
	BaseStation      string    `json:"base_station"`
}

var beacons []Beacon

func init() {
	d, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal("Can't get running directory:", err)
	}
	dir = flag.String("dir", d, "directory where the public directory is in")
	dur = flag.Duration("d", 5*time.Second, "Scan duration")
	port = flag.Int("p", 23232, "the port where the server starts")
	flag.Parse()
}

func main() {
	f, err := os.OpenFile("blueblue.log",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
	}
	defer f.Close()
	logger = log.New(f, "", log.LstdFlags)

	d, err := linux.NewDevice()
	if err != nil {
		logger.Fatal("Can't create new device:", err)
	}
	ble.SetDefaultDevice(d)
	serve()
}

// handle the advertisement
func advHandler(a ble.Advertisement) {
	if len(a.ServiceData()) > 0 {
		svcdata := a.ServiceData()
		for _, s := range svcdata {
			// Radioland beacon service UUID
			if s.UUID[0] == 0x03 && s.UUID[1] == 0x18 {
				now := time.Now()
				beacon := Beacon{
					MAC:              a.Addr().String(),
					Detected:         now,
					Name:             a.LocalName(),
					UUID:             getUUID(a.ManufacturerData()),
					Major:            getMajor(a.ManufacturerData()),
					Minor:            getMinor(a.ManufacturerData()),
					RSSI:             a.RSSI(),
					ManufacturerData: hex.EncodeToString(a.ManufacturerData()),
					ServiceUUID:      hex.EncodeToString(s.UUID),
					ServiceData:      hex.EncodeToString(s.Data),
					Battery:          getBatteryLevel(a.ManufacturerData()),
					Temperature:      "0",
					BaseStation:      "Pi4",
				}
				beacons = append(beacons, beacon)
			}
		}
	}
}

// get UUID
func getUUID(md []byte) (uuid string) {
	// byte 4 - 20 (16 bytes)
	uuid = hex.EncodeToString(md[4:20])
	return
}

func getMajor(md []byte) (major string) {
	// byte 21 - 23 (2 bytes)
	major = hex.EncodeToString(md[21:23])
	return
}

func getMinor(md []byte) (minor string) {
	// byte 24 - 26 (2 bytes)
	minor = hex.EncodeToString(md[24:26])
	return
}

func getBatteryLevel(md []byte) (batt string) {
	// last byte
	batt = fmt.Sprintf("%x", md[len(md)-1])
	return
}

// start the web server
func serve() {
	mux := http.NewServeMux()
	mux.Handle("/public/", http.StripPrefix("/public/", http.FileServer(http.Dir(*dir+"/public"))))
	mux.HandleFunc("/", index)
	mux.HandleFunc("/stop", stopScan)
	mux.HandleFunc("/start", startScan)
	mux.HandleFunc("/beacons", beaconsJSON)
	server := &http.Server{
		Addr:    "0.0.0.0:" + strconv.Itoa(*port),
		Handler: mux,
	}
	fmt.Println("Started blueblue server at", server.Addr)
	server.ListenAndServe()
}

func filterByDate(beacons []Beacon, sec int) (results []Beacon) {
	for _, beacon := range beacons {
		t := time.Now().Add(-1 * time.Duration(sec) * time.Second)
		if t.Before(beacon.Detected) {
			results = append(results, beacon)
		}
	}
	return
}

// index for web server
func index(w http.ResponseWriter, r *http.Request) {
	t, _ := template.ParseFiles(*dir + "/public/index.html")
	t.Execute(w, strconv.Itoa(*port))
}

// show a JSON of beacons
func beaconsJSON(w http.ResponseWriter, r *http.Request) {
	lastParam := r.URL.Query().Get("last")
	if lastParam == "" {
		lastParam = "60"
	}
	last, err := strconv.Atoi(lastParam)
	if err != nil {
		t, _ := template.ParseFiles(*dir + "/public/error.html")
		t.Execute(w, err)
	}
	filtered := filterByDate(beacons, last)
	str, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		t, _ := template.ParseFiles(*dir + "/public/error.html")
		t.Execute(w, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(str))
}

// handler to start scanning
func startScan(w http.ResponseWriter, r *http.Request) {
	if !stop {
		w.WriteHeader(409)
		w.Write([]byte("Already scanning."))
	} else {
		go scan()
		w.Write([]byte("Request to start scanning accepted."))
	}
}

// handler to stop scanning
func stopScan(w http.ResponseWriter, r *http.Request) {
	if stop {
		w.WriteHeader(409)
		w.Write([]byte("Already stopped."))
	} else {
		stop = true
		w.Write([]byte("Request to stop scanning accepted."))
	}
}

// scan goroutine
func scan() {
	stop = false
	logger.Println("Started scanning every", *dur)
	for !stop {
		ctx := ble.WithSigHandler(context.WithTimeout(context.Background(), *dur))
		check(ble.Scan(ctx, true, advHandler, nil))
	}
	logger.Println("Stopped scanning.")
	stop = true
}

// check the BLE scan for errors
func check(err error) {
	switch errors.Cause(err) {
	case nil:
	case context.DeadlineExceeded:
		logger.Println("Scan complete.")
	case context.Canceled:
		logger.Println("Scan canceled.")
	default:
		logger.Fatal(err.Error())
	}
}
