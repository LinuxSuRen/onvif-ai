package onvif

import "time"

type Config struct {
	DeviceAddr string
	Username   string
	Password   string
	Timeout    time.Duration
}

type Profile struct {
	Token string
	Name  string
}

type StreamURI struct {
	URI                string
	Timeout            string
	InvalidAfterConnect bool
	InvalidAfterReboot  bool
}

type SnapshotURI struct {
	URI string
}

type DeviceInformation struct {
	Manufacturer    string
	Model           string
	FirmwareVersion string
	SerialNumber    string
	HardwareID      string
}
