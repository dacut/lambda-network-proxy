package event

type ProxyEndpointEvent struct {
	ClientProtocol string `json:"ClientProtocol"`
	ClientAddress  string `json:"ClientAddress"`
	ClientPort     uint   `json:"ClientPort"`
	ProxyProtocol  string `json:"ProxyProtocol"`
	ProxyAddress   string `json:"ProxyAddress"`
	ProxyPort      uint   `json:"ProxyPort"`
}
