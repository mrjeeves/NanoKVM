# CEC logo for the NanoKVM

The CEC brand mark (the "critical error" warning triangle) for the two places a
NanoKVM shows a logo, per the wiki's "Customizing Logo" flow
(https://wiki.sipeed.com/hardware/en/kvm/NanoKVM/development.html):

- **OLED screen** — `logo.bin` (32 bytes, 16×16 monochrome). The kvm_system
  firmware reads `/boot/logo.bin` and shows it in place of the built-in Sipeed
  logo (`support/sg2002/kvm_system/main/lib/oled_ctrl/oled_ctrl.cpp`). `just
  deploy` ships this file to `/boot/logo.bin`.
- **Web management UI** — the login/favicon logo is embedded directly in the web
  build, so it ships on every device we build the web for and needs no
  `/boot/logo.ico`. It lives at `web/public/sipeed.ico` — the **upstream
  filename is deliberately kept**: `S95nanokvm` swaps that exact path against
  `/boot/logo.ico` for the custom-logo flow, and the login page loads it by
  path. Only the bytes are ours. It carries 16/32/48 px entries plus a 180 px
  PNG entry, because the login page renders it at its natural size.

`logo.bin` is a hand-tuned 16×16 glyph of the mark — the auto-downscale of the
full logo turns to mush at 16×16 monochrome, and the wordmark and the two-tone
cyan/magenta fill carry no information once it's 1-bit. What survives the
reduction is the silhouette everyone reads as *critical error*: the triangle
outline with the exclamation inside.

```
·······##·······
·······##·······
······#··#······
······#··#······
·····#·##·#·····
·····#·##·#·····
····#··##··#····
····#··##··#····
···#···##···#···
···#········#···
··#····##····#··
··#····##····#··
·#············#·
·##############·
```

Source art: `assets/cec-logo.png` in the
[support.cec.direct](https://github.com/mrjeeves/support.cec.direct) repo. The
32 bytes are packed exactly as `binary_to_bytes()` in `../logo_generator.py`
does it — column-major, two 8-row pages per column, LSB the topmost row — so
the glyph can be round-tripped back through the editor for a tweak.
