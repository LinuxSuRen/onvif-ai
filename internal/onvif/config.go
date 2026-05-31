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

func DefaultConfig() Config {
	return Config{
		DeviceAddr: "192.168.1.138:8089/onvif/device_service",
		Timeout:    10 * time.Second,
	}
}
