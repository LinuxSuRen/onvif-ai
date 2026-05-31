package onvif

const (
	deviceNamespace  = "http://www.onvif.org/ver10/device/wsdl"
	mediaNamespace   = "http://www.onvif.org/ver10/media/wsdl"
	ptzNamespace     = "http://www.onvif.org/ver20/ptz/wsdl"
	imagingNamespace = "http://www.onvif.org/ver20/imaging/wsdl"
	deviceServiceURL = "http://www.onvif.org/ver10/device/wsdl"
)

type Capabilities struct {
	Device  *DeviceCapabilities  `xml:"GetCapabilitiesResponse>Device"`
	Media   *MediaCapabilities   `xml:"GetCapabilitiesResponse>Media"`
	PTZ     *PTZCapabilities     `xml:"GetCapabilitiesResponse>PTZ"`
	Imaging *ImagingCapabilities `xml:"GetCapabilitiesResponse>Imaging"`
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
