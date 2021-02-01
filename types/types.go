package types

type HealthCheck struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	Message string `json:"messgae"`
}

type Config struct {
	Type        string `json:"type"`
	HealthCheck string `json:"health_check"`
}