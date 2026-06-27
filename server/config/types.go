package config

type Config struct {
	Proto          string   `yaml:"proto"`
	Host           string   `yaml:"host"`
	Port           Port     `yaml:"port"`
	Cert           Cert     `yaml:"cert"`
	Logger         Logger   `yaml:"logger"`
	Authentication string   `yaml:"authentication"`
	JWT            JWT      `yaml:"jwt"`
	Stun           string   `yaml:"stun"`
	Turn           Turn     `yaml:"turn"`
	Security       Security `yaml:"security"`
	Mesh           Mesh     `yaml:"mesh"`

	Hardware Hardware `yaml:"-"`
}

// Mesh configures the native AllMyStuff bridge — the daemon-socket client that
// joins the AllMyStuff cloud mesh, advertises this device as a KVM appliance,
// and tunnels its own web UI over the mesh "sites" plane.
type Mesh struct {
	// Enabled turns the bridge on. Default true; the bridge is non-fatal and
	// retries on connect failure, so it's safe to leave on even before the
	// myownmesh daemon is up.
	Enabled bool `yaml:"enabled"`
	// Home is $MYOWNMESH_HOME — where the daemon's control socket
	// (daemon.sock) and our persisted KVM state (kvm-state.json) live.
	Home string `yaml:"home"`
	// NetworkId is the wire-level rendezvous handle of the network we join.
	NetworkId string `yaml:"networkId"`
	// Label is the cosmetic display name for that network.
	Label string `yaml:"label"`
	// Relays is the explicit signaling relay list. Empty means use the public
	// venue default (the daemon's built-in relays).
	Relays []string `yaml:"relays"`
	// DaemonBin is the best-guess path to the myownmesh daemon binary, used by
	// the init script — not by the Go bridge directly.
	DaemonBin string `yaml:"daemonBin"`
}

type Logger struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type Port struct {
	Http  int `yaml:"http"`
	Https int `yaml:"https"`
}

type Cert struct {
	Crt string `yaml:"crt"`
	Key string `yaml:"key"`
}

type JWT struct {
	SecretKey            string `yaml:"secretKey"`
	RefreshTokenDuration uint64 `yaml:"refreshTokenDuration"`
	RevokeTokensOnLogout bool   `yaml:"revokeTokensOnLogout"`
}

type Turn struct {
	TurnAddr string `yaml:"turnAddr"`
	TurnUser string `yaml:"turnUser"`
	TurnCred string `yaml:"turnCred"`
}

type Security struct {
	LoginLockoutDuration int `yaml:"loginLockoutDuration"`
	LoginMaxFailures     int `yaml:"loginMaxFailures"`
}

type Hardware struct {
	Version      HWVersion `yaml:"-"`
	GPIOReset    string    `yaml:"-"`
	GPIOPower    string    `yaml:"-"`
	GPIOPowerLED string    `yaml:"-"`
	GPIOHDDLed   string    `yaml:"-"`
}
