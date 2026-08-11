# NanoKVM kernel compatibility modules

Remote boot media uses FUSE to expose a lazily fetched image to the USB mass
storage gadget. The shipping standard NanoKVM kernel (`5.10.4-tag-`) enables
loadable modules but does not include FUSE or POSIX ACL support.

`kvmapp/system/ko/fuse.ko` was built from Sipeed's
`LicheeRV-Nano-Build` kernel tree at commit
`d4003f15b35d43ad4842f427050ab2bba0114fa5`, using the running device's
`/proc/config.gz`, `CONFIG_FUSE_FS=m`, `CONFIG_FS_POSIX_ACL=n`, and
`LOCALVERSION=-tag-`. Apply `patches/fuse-no-posix-acl.patch` before running:

```sh
make ARCH=riscv LOCALVERSION=-tag- \
  CROSS_COMPILE=/opt/host-tools/gcc/riscv64-linux-musl-x86_64/bin/riscv64-unknown-linux-musl- \
  syncconfig
make ARCH=riscv LOCALVERSION=-tag- \
  CROSS_COMPILE=/opt/host-tools/gcc/riscv64-linux-musl-x86_64/bin/riscv64-unknown-linux-musl- \
  M=fs/fuse clean modules
```

Expected artifact SHA-256:

```text
303fd0c284acbac00d2355fadb5b6e0b6c02a03e18dc4f10ce98f2dbb80ce1c0
```

The module was hardware-tested on a standard NanoKVM running
`5.10.4-tag-`: it loaded without force flags, created `/dev/fuse`, served
nonzero random reads at the beginning, middle, and end of a virtual image, and
backed the KVM USB mass-storage function read-only.

