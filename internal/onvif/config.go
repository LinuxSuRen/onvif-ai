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
	// PTZToken is non-empty when the profile carries a PTZConfiguration,
	// i.e. the camera exposes pan/tilt/zoom control on this profile.
	PTZToken string
}

// HasPTZ reports whether this media profile supports PTZ control.
func (p Profile) HasPTZ() bool {
	return p.PTZToken != ""
}

type StreamURI struct {
	URI                 string
	Timeout             string
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
