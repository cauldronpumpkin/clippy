package app

const (
	defaultServiceName   = "_clippy._tcp"
	defaultDomain        = "local."
	defaultWebSocketPath = "/ws"
)

// Config controls daemon startup and runtime wiring.
type Config struct {
	Name            string
	ListenAddr      string
	Port            int
	ServiceName     string
	Domain          string
	WebSocketPath   string
	ControlEndpoint string
}

// WithDefaults fills in zero values with the app defaults.
func (c Config) WithDefaults() Config {
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName
	}
	if c.Domain == "" {
		c.Domain = defaultDomain
	}
	if c.WebSocketPath == "" {
		c.WebSocketPath = defaultWebSocketPath
	}
	return c
}
