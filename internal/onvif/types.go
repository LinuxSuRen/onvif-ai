package onvif

import "encoding/xml"

const (
	deviceNamespace  = "http://www.onvif.org/ver10/device/wsdl"
	mediaNamespace   = "http://www.onvif.org/ver10/media/wsdl"
	ptzNamespace     = "http://www.onvif.org/ver20/ptz/wsdl"
	imagingNamespace = "http://www.onvif.org/ver20/imaging/wsdl"
	deviceServiceURL = "http://www.onvif.org/ver10/device/wsdl"
)

type Capabilities struct {
	XMLName xml.Name              `xml:"GetCapabilitiesResponse"`
	Device  *DeviceCapabilities  `xml:"Device"`
	Media   *MediaCapabilities   `xml:"Media"`
	PTZ     *PTZCapabilities     `xml:"PTZ"`
	Imaging *ImagingCapabilities `xml:"Imaging"`
}

type DeviceCapabilities struct {
	XAddr string `xml:"XAddr"`
}

type MediaCapabilities struct {
	XAddr string `xml:"XAddr"`
}

type PTZCapabilities struct {
	XAddr string `xml:"XAddr"`
}

type ImagingCapabilities struct {
	XAddr string `xml:"XAddr"`
}
