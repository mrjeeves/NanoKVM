package config

var defaultConfig = &Config{
	Proto: "http",
	Host:  "",
	Port: Port{
		Http:  80,
		Https: 443,
	},
	Cert: Cert{
		Crt: "server.crt",
		Key: "server.key",
	},
	Logger: Logger{
		Level: "info",
		File:  "stdout",
	},
	JWT: JWT{
		SecretKey:            "",
		RefreshTokenDuration: 2678400,
		RevokeTokensOnLogout: true,
	},
	Stun: "stun.l.google.com:19302",
	Turn: Turn{
		TurnAddr: "",
		TurnUser: "",
		TurnCred: "",
	},
	Authentication: "enable",
	Security: Security{
		LoginLockoutDuration: 0,
		LoginMaxFailures:     5,
	},
	Mesh: Mesh{
		Enabled:   true,
		Home:      "/data/myownmesh",
		NetworkId: "cec-backend-client-mesh",
		Label:     "CEC Backend Client Mesh",
		Relays:    nil,
		DaemonBin: "/kvmapp/system/bin/myownmesh",
	},
}

func checkDefaultValue() {
	if instance.JWT.SecretKey == "" {
		instance.JWT.SecretKey = generateRandomSecretKey()
		instance.JWT.RevokeTokensOnLogout = true
	}

	if instance.JWT.RefreshTokenDuration == 0 {
		instance.JWT.RefreshTokenDuration = 2678400
	}

	if instance.Stun == "" {
		instance.Stun = "stun.l.google.com:19302"
	}

	if instance.Authentication == "" {
		instance.Authentication = "enable"
	}

	// Fill mesh defaults for a config.yaml written before the mesh block
	// existed (viper leaves the zero value otherwise). We can't distinguish a
	// user-set Enabled:false from an absent block, so only the string fields
	// are defaulted — Enabled defaults via the on-disk default config above.
	if instance.Mesh.Home == "" {
		instance.Mesh.Home = "/data/myownmesh"
	}
	if instance.Mesh.NetworkId == "" {
		instance.Mesh.NetworkId = "cec-backend-client-mesh"
	}
	if instance.Mesh.Label == "" {
		instance.Mesh.Label = "CEC Backend Client Mesh"
	}
	if instance.Mesh.DaemonBin == "" {
		instance.Mesh.DaemonBin = "/kvmapp/system/bin/myownmesh"
	}

	instance.Hardware = getHardware()
}
