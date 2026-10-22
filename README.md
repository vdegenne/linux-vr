# linux-vr

A fork of [vr-meta/linux-vr](https://github.com/vr-meta/linux-vr) for streaming a Linux desktop to a Meta Quest headset.

This fork adds a `wlr-randr` fallback for monitor detection, extending compatibility beyond GNOME/Mutter to other Wayland compositors.

## Prerequisites

- A working Wayland session with DRM/KMS support.
- FFmpeg with `kmsgrab` and a compatible H.264 hardware encoder.
- `wlr-randr` for fallback monitor detection.
- A Meta Quest headset on the same local network.

### Hardware encoding

Check the available VA-API profiles:

```bash
vainfo
```

If the required H.264 encoding profile is missing on Fedora, install the freeworld Mesa drivers:

```bash
sudo dnf install mesa-va-drivers-freeworld.x86_64
```

Run `vainfo` again to verify that the required encoding profile is available.

## Build/Install/Start (server)

Install Go and the required system dependencies, then clone this fork:

```bash
git clone <your-fork-url>
cd linux-vr
```

Build and install the server and its systemd user service:

```bash
make server
make install
```

Alternatively, install the server binary to your local bin directory:

```bash
make install-server BINDIR="$HOME/.local/bin"
```

If installed as a user service, start or restart it with:

```bash
systemctl --user restart linux-vr-server
```

Check its logs if something does not work:

```bash
journalctl --user -u linux-vr-server -f
```

The server interface is available at `http://localhost:9099`.

## Build/Install APK (headset)

Install JDK 21 and Android SDK 34. Create `client/local.properties` and set your Android SDK path:

```properties
sdk.dir=/path/to/Android/Sdk
```

Build the APK from the repository root:

```bash
make client
```

Enable Developer Mode and USB debugging on the Quest, connect it to your computer, and authorize the debugging prompt. Then install the APK:

```bash
adb devices
adb install -r client/app/build/outputs/apk/debug/app-debug.apk
```

Open the app from the headset's app library under **Unknown Sources**.

## Troubleshooting

If monitor detection fails, run `wlr-randr` and check the server logs. Successful monitor detection does not guarantee that KMS capture or hardware encoding will work.

The video encoder starts only when a screen window is opened in the headset and stops when that window closes. The server service can remain running without an active encoder.

For the original project and further details, see the [upstream repository](https://github.com/vr-meta/linux-vr).
