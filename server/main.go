package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"NanoKVM-Server/common"
	"NanoKVM-Server/config"
	"NanoKVM-Server/logger"
	"NanoKVM-Server/middleware"
	"NanoKVM-Server/router"
	"NanoKVM-Server/service/button"
	"NanoKVM-Server/service/mesh"
	"NanoKVM-Server/service/mesh/glue"
	"NanoKVM-Server/service/vm/jiggler"
	"NanoKVM-Server/utils"

	"github.com/gin-gonic/gin"
	cors "github.com/rs/cors/wrapper/gin"
)

func main() {
	initialize()
	defer dispose()

	run()
}

func initialize() {
	if err := config.EnsurePicoclawInternalToken(); err != nil {
		log.Fatalf("failed to initialize picoclaw internal token: %v", err)
	}

	logger.Init()

	// init screen parameters
	_ = common.GetScreen()

	// init HDMI
	vision := common.GetKvmVision()
	vision.SetHDMI(false)
	time.Sleep(10 * time.Millisecond)
	if !utils.IsHdmiDisabled() {
		vision.SetHDMI(true)
	}

	// run mouse jiggler
	jiggler.GetJiggler().Run()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		sig := <-sigChan
		log.Printf("\nReceived signal: %v\n", sig)

		dispose()
		os.Exit(0)
	}()
}

func run() {
	conf := config.GetInstance()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	if conf.Authentication == "disable" {
		r.Use(cors.AllowAll())
	}

	// Give mesh-tunneled requests a session cookie so the web UI's client-side
	// login gate treats a mesh-authorized viewer as logged in (no KVM password).
	// Registered before router.Init so it sets the cookie on the SPA's own HTML
	// response; direct LAN requests are never mesh-marked, so they're unaffected.
	r.Use(middleware.MeshSessionCookie())

	router.Init(r)

	// Start the AllMyStuff mesh bridge (native integration). Non-fatal: it
	// retries on connect failure, since the myownmesh daemon may not be up yet.
	// The /api/mesh routes are mounted either way so the web UI's Mesh tab can
	// report a disabled bridge instead of erroring.
	var bridge *mesh.Bridge
	if conf.Mesh.Enabled {
		bridge = mesh.NewBridge(r, conf)
		// Wire the native (Slice 1) screen/HID path: the bridge is CGO-free, so
		// its H.264 encoder and HID gadget arrive as injected interfaces from the
		// on-device glue. A display route then streams the KVM's screen and an
		// input route injects its keyboard/mouse.
		bridge.SetVideoSource(glue.NewVideoSource())
		bridge.SetInputSink(glue.NewInputSink())
		go bridge.Start(make(chan struct{}))
		log.Println("AllMyStuff mesh bridge started")

		// Wire the physical BOOT button to the CEC hand-raise. We grab the input
		// node so the screen firmware's own gestures (OLED nav, WiFi hotspot)
		// stop firing and the button does one thing — a tap raises/lowers the
		// hand. The firmware's hold-to-reset is the one gesture worth keeping, so
		// we re-implement it here (the grab took it from the firmware). Non-fatal
		// and self-disabling if the input node isn't present.
		button.Watch(button.Config{
			Enabled:        conf.Mesh.HandRaise.ButtonEnabled,
			Device:         conf.Mesh.HandRaise.InputDevice,
			KeyCode:        conf.Mesh.HandRaise.KeyCode,
			Grab:           true,
			OnFactoryReset: resetKvmAccount,
		}, bridge)
	}
	mesh.RegisterRoutes(r, bridge)

	httpAddr := utils.ListenAddr(conf.Host, strconv.Itoa(conf.Port.Http))
	loopbackHTTPAddr := utils.ListenAddr("127.0.0.1", strconv.Itoa(conf.Port.Http))
	needsLoopbackHTTP := utils.NeedsDedicatedLoopbackListener(conf.Host)

	if conf.Proto == "https" {
		httpsPortStr := strconv.Itoa(conf.Port.Https)

		go func() {
			err := r.RunTLS(utils.ListenAddr(conf.Host, httpsPortStr), conf.Cert.Crt, conf.Cert.Key)
			if err != nil {
				panic("start https server failed")
			}
		}()

		if needsLoopbackHTTP {
			go func() {
				if err := middleware.ListenAndServeLoopbackHTTPRedirect(
					loopbackHTTPAddr,
					httpsPortStr,
					r,
					router.LoopbackHTTPAllowedPaths()...,
				); err != nil {
					panic("start loopback http server failed")
				}
			}()
		}

		if err := middleware.ListenAndServeLoopbackHTTPRedirect(
			httpAddr,
			httpsPortStr,
			r,
			router.LoopbackHTTPAllowedPaths()...,
		); err != nil {
			panic("start http server failed")
		}
	} else {
		if needsLoopbackHTTP {
			go func() {
				if err := r.Run(loopbackHTTPAddr); err != nil {
					panic("start loopback http server failed")
				}
			}()
		}

		if err := r.Run(httpAddr); err != nil {
			panic("start http server failed")
		}
	}
}

func dispose() {
	common.GetKvmVision().Close()
}

// resetKvmAccount reproduces the on-device firmware's hold-to-reset
// (kvm_system's kvm_reset_password): it sets root's password back to "root" and
// removes the web UI account file, then syncs. Because the button watcher grabs
// the input node, the firmware no longer runs this itself — so a long hold of
// the BOOT button lands here instead. The command sequence is fed to a shell
// exactly like the firmware's popen(bash) so `passwd` reads its two
// confirmations from the same stdin.
func resetKvmAccount() {
	const script = "passwd root\nroot\nroot\nrm -f /etc/kvm/pwd\nsync\n"
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("button: factory reset failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return
	}
	log.Println("button: factory reset done — root password reset to 'root', /etc/kvm/pwd removed")
}
