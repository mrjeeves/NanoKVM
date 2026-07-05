# AllMyStuff logo for the NanoKVM

The AllMyStuff brand mark (three connected nodes) for the two places a NanoKVM
shows a logo, per the wiki's "Customizing Logo" flow
(https://wiki.sipeed.com/hardware/en/kvm/NanoKVM/development.html):

- **OLED screen** — `logo.bin` (32 bytes, 16×16 monochrome). The kvm_system
  firmware reads `/boot/logo.bin` and shows it in place of the built-in Sipeed
  logo (`support/sg2002/kvm_system/main/lib/oled_ctrl/oled_ctrl.cpp`). `just
  deploy` ships this file to `/boot/logo.bin`.
- **Web management UI** — the login/favicon logo is embedded directly in the web
  build (`web/public/sipeed.ico`, the AllMyStuff app icon), so it ships on every
  device we build the web for and needs no `/boot/logo.ico`.

`logo.bin` is a hand-tuned 16×16 glyph of the mark (the auto-downscale of the
full logo turns to mush at 16×16 monochrome). It's a lit 3-node triangle on the
dark OLED. The source art is AllMyStuff's `gui/src-tauri/icons/icon.png`.
