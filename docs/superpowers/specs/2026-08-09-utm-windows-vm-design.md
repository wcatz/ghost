# UTM-native Windows 11 VM for headless agent testing

## Context

Ghost's MCP server is being tested on Windows. An earlier attempt drove a
Windows 11 ARM64 guest with QEMU/TCG (pure emulation) from a headless SSH
session, using a QEMU HMP monitor socket + screendump + OCR to interact with
the guest. That install succeeded (desktop reached, `ghost` user created,
OpenSSH Server install started), but TCG made the guest brutally slow:
`Add-WindowsCapability` for OpenSSH.Server took over an hour of wall-clock
time and was still running.

The host (Apple M1, macOS 26.6.1) has Hypervisor.framework available
(`kern.hv_support: 1`), which UTM uses to run aarch64 guests at near-native
speed. The previous install's slow, fragile TCG setup is replaced by a
UTM-native VM configuration that uses HVF acceleration and is launched
headlessly over SSH (no desktop, ever — this is only for agents to test
ghost).

## Goal

A new UTM VM bundle (`ghost-win11-utm.utm`) that:

- reuses the existing installed disk `win11.qcow2` (no reinstall);
- is accelerated by HVF, not TCG;
- is headless-only (never launched as a desktop window);
- exposes SSH on host port 2222 so agents can run ghost inside Windows;
- exposes RDP on host port 3389 as an optional escape hatch.

## Key design decisions

1. **Reuse the installed qcow2.** Windows 11 25H2 ARM64 is already installed
   and past OOBE on `/Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2`.
   Reinstalling would repeat hours of emulation-time. The disk is untouched
   by this change; the new bundle points at it.

2. **Disk must stay NVMe.** The installed Windows bound the inbox `stornvme`
   driver to the system disk. UTM's default VirtIO disk interface would not
   boot this image. The new bundle sets the drive interface to `NVMe`
   (supported by UTM 4.7.5).

3. **HVF acceleration.** `QEMU.Hypervisor = true`, CPU `max`, 8 cores, 8 GB
   RAM, `virt` machine. Verified: `-accel hvf -cpu max` initializes fine.
   `pauth-impdef=on` was a TCG-only workaround and must NOT be carried into
   the HVF launch (it errors under HVF).

4. **Headless launch from SSH.** UTM's GUI is not used. The bundle's
   `config.plist` is the source of truth; the VM is launched with brew QEMU
   (`-accel hvf`) plus the same HMP monitor socket + screendump workflow,
   so the guest is operated exactly as the TCG VM was.

5. **Keep `answer.iso` attached.** It carries NetKVM + the other virtio
   drivers and `utm-guest-tools-0.1.271.exe`, needed if the virtio-net NIC
   did not enumerate during install. Detach the Windows install ISO (its
   boot priority caused repeated EFI-shell detours during install; the
   install is done).

## Steps

1. Cleanly stop the current TCG VM (pid 50552) via HMP `system_powerdown`,
   waiting for exit, so the qcow2 is not locked and is in a consistent
   state.
2. Create `/Users/wayne/vms/ghost-win11-utm.utm/` with a `config.plist`
   specifying: HVF, NVMe drive pointing at the existing qcow2, virtio-net
   with hostfwd `2222→22`/`3389→3389`, `answer.iso` as a USB CD, no install
   ISO, display `virtio-ramfb` (present for UTM compatibility; never opened
   as a window).
3. Launch headless with brew QEMU:
   `-accel hvf -cpu max`, NVMe system drive, virtio-net hostfwd, HMP monitor
   on `/tmp/w11utm-mon.sock`, no display.
4. On first boot, finish the interrupted OpenSSH Server install
   (`Add-WindowsCapability` + `Start-Service sshd` + firewall rule), which
   is fast under HVF. If the virtio-net NIC did not enumerate, install
   NetKVM from `answer.iso` and retry.
5. Verify: `ssh ghost@localhost -p 2222` works; run `ghost mcp init` in the
   guest to confirm the original testing goal.

## Non-goals

- No desktop/display session for humans — agents only.
- No reinstall of Windows.
- No changes to UTM's own bundled QEMU or the installed `win11.qcow2`
  contents beyond finishing the OpenSSH setup.

## Risks

- The virtio-net driver may not be bound yet; NetKVM on `answer.iso` is the
  fallback and is quick to apply under HVF.
- Interrupting the in-flight DISM OpenSSH install when stopping the TCG VM
  could leave the capability half-installed; re-running
  `Add-WindowsCapability` is idempotent and recovers it.
