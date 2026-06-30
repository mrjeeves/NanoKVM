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
	// Name is the device's display name advertised on the mesh (the graph
	// label). Defaults to "CEC-KVM". Empty falls back to the hostname/node id.
	Name string `yaml:"name"`
	// Home is $MYOWNMESH_HOME — where the daemon's identity, rosters, and our
	// persisted KVM state (kvm-state.json) live. This is on the device's
	// writable data partition (/data), which is often exFAT/FAT — fine for
	// regular files, but it cannot hold a Unix socket (see Socket).
	Home string `yaml:"home"`
	// Socket is the daemon control socket path. It must live on a filesystem
	// that supports Unix sockets — exFAT/FAT (the typical /data partition) does
	// not (bind() returns EPERM), so the default is on tmpfs (/tmp). The daemon
	// is pointed at this same path via $Home/config.json by the init script; the
	// two must match. Empty falls back to $Home/daemon.sock.
	Socket string `yaml:"socket"`
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
