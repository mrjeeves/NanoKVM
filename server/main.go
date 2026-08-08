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

	"NanoKVM-Server/buildinfo"
	"NanoKVM-Server/common"
	"NanoKVM-Server/config"
	"NanoKVM-Server/logger"
	"NanoKVM-Server/middleware"
	"NanoKVM-Server/router"
	"NanoKVM-Server/service/application"
	"NanoKVM-Server/service/button"
	"NanoKVM-Server/service/mesh"
	"NanoKVM-Server/service/mesh/glue"
	"NanoKVM-Server/service/storage"
	"NanoKVM-Server/service/viewer"
	"NanoKVM-Server/service/vm"
	"NanoKVM-Server/service/vm/jiggler"
	"NanoKVM-Server/utils"

	"github.com/gin-gonic/gin"
	cors "github.com/rs/cors/wrapper/gin"
)

// hdmiSettle is how long the receiver is given to re-negotiate with the source
// after HPD is re-asserted. Measured against a cold hot-plug, not a warm one:
// the source's own EDID read and mode set is the slow half, and it is the same
// work a physical replug triggers.
const hdmiSettle = 1500 * time.Millisecond

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

	// Keep HDMI present only while a web or mesh-native screen viewer holds a
	// lease. The operator's persisted preference is seeded FIRST and outranks
	// the lease: /api/vm/hdmi still reports that setting to the UI, so a lease
	// that ignored it left the switch reading "on" over a dark receiver.
	vision := common.GetKvmVision()
	viewer.SetAllowed(!utils.IsHdmiDisabled())
	viewer.Configure(func(active bool) {
		vision.SetHDMI(active)
		if active {
			// Re-asserting HPD makes the source re-negotiate the link; the
			// encoder has nothing to read until it does. Hold the activation
			// open across that so the first viewer's first frame lands on a
			// live link instead of tripping the "No image captured" path. The
			// original 20ms was three orders of magnitude short of what an
			// HDMI source needs after a hot-plug.
			time.Sleep(hdmiSettle)
		}
	})

	// S03usbdev decides whether to export the drive at S03, before the server
	// that writes the image has run. Re-check now that it exists, so the drive
	// appears on the boot the image lands rather than the one after.
	storage.EnsureDriveExported()

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

		// Converge onto the rest of this release's payload — the pinned daemon
		// and the boot scripts. A device updated from an older server is updated
		// by that older server's updater, which installs only the parts it knows
		// about, so the new server boots with pieces of its own release missing.
		// This heals that once per version, out of band, without a manual
		// deploy. A no-op on an ordinary boot once the marker is set.
		go application.ReconcileRelease(buildinfo.Version, conf.Mesh.DaemonBin, conf.Mesh.Home)

		// Retire an RNDIS USB gadget in favour of NCM. Unconditional and local:
		// unlike the reconcile above it needs no network, no release tag and no
		// marker, so it also converges a dev build and a device whose reconcile
		// has already run. Windows 11 ships no in-box RNDIS driver and macOS
		// never had one, so an RNDIS flag is a dead configuration rather than a
		// setting — and a device still carrying one is exactly the device that
		// needs the fix. A stat-and-return once migrated; absence of both flags
		// still means "off", which IS a choice, and is left alone.
		application.MigrateUsbNetworkFlag()

		// A device nobody has claimed yet needs to be reachable before it has a
		// network — which is the whole difficulty, since getting it onto one is
		// often what the KVM is for. Bringing the USB network gadget up makes the
		// machine it's physically plugged into able to claim it over the cable,
		// with no LAN, no router and no credentials. Once per device and never
		// undone; see EnsureUsbNetworkForClaim. Off the startup path because it
		// shells out and bounces the USB gadget.
		go vm.EnsureUsbNetworkForClaim(bridge.Claimed(), conf.Mesh.Home)

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
