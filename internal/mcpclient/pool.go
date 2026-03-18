package mcpclient

// Pool manages a collection of MCP server configurations.
//
// Ephemeral servers (those supplied per-request via the Agent Run API) are
// added to a temporary pool that is torn down when the request completes.
// Persistent servers (those defined in the application configuration) are
// held open for the lifetime of the process.
//
// The zero value of Pool is not usable; use New to obtain a ready Pool.
type Pool struct {
	servers []ServerConfig
}

// New returns an empty Pool ready to use.
func New() *Pool {
	return &Pool{}
}

// Add appends a server configuration to the pool. Connections are not
// established until they are actually needed.
func (p *Pool) Add(cfg ServerConfig) {
	p.servers = append(p.servers, cfg)
}

// Servers returns a copy of the server configurations held by the pool.
func (p *Pool) Servers() []ServerConfig {
	out := make([]ServerConfig, len(p.servers))
	copy(out, p.servers)
	return out
}

// Close discards all server configurations from the pool. It is safe to
// call Close on a nil Pool.
func (p *Pool) Close() {
	p.servers = nil
}

// ServerConfig describes a single MCP server connection.
//
// Transport defaults to "sse" when the field is empty. Use
// "streamable-http" for the HTTP streaming variant of the Model Context
// Protocol.
type ServerConfig struct {
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}
