# Web-builder image: Node 22 (vite 7 needs Node >=20) + the toolchain node-gyp
# needs. The web deps pull in optional native addons for `ws` (bufferutil,
# utf-8-validate) whose install scripts invoke node-gyp; the stock node:22-slim
# image has no python3/g++, so building them fails and `pnpm install` aborts the
# whole install — even though those addons are runtime WebSocket perf shims that
# a vite BUILD never touches. Baking python3/build-essential in lets them
# compile, and caching this as an image keeps repeat `just build-web` runs fast
# (no re-apt).
#
# No --platform pin: the vite output is plain JS, so the native host arch builds
# the exact same bytes (and native is faster than emulation on a Mac).
FROM node:22-bookworm-slim

# build-essential (gcc, g++, make, libc6-dev) + python3 = everything node-gyp
# needs to compile a native addon; cherry-picking g++/make can miss libc headers.
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 build-essential \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g pnpm@10

WORKDIR /web
