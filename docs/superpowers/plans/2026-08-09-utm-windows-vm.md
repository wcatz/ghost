# UTM-Native Windows 11 VM for Headless Agent Testing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the slow TCG-based Windows 11 ARM64 test VM with a UTM-native, HVF-accelerated, headless-only VM that reuses the existing installed disk and exposes SSH on host port 2222.

**Architecture:** A new UTM bundle (`ghost-win11-utm.utm`) whose `config.plist` is the source of truth (HVF accel, NVMe disk, virtio-net, hostfwd 2222/3389). The VM is launched headlessly with brew QEMU using `-accel hvf`, operated via the same HMP monitor + screendump + OCR workflow used for the TCG VM. `answer.iso` stays attached to supply NetKVM/guest tools if the virtio NIC did not enumerate.

**Tech Stack:** brew QEMU 11.0.3 (`qemu-system-aarch64`), UTM 4.7.5 bundle format (plist), Hypervisor.framework (HVF), Swift Vision OCR helper `/tmp/ocr.swift`, HMP unix-socket monitor.

## Global Constraints

- Host: Apple M1, macOS 26.6.1; shell is zsh; remote work via SSH (`wayne@notty`).
- MUST reuse existing installed disk `/Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2` (Windows 11 25H2 ARM64, `ghost`/`GhostTest!2026`, desktop reached, OpenSSH Server install interrupted mid-DISM).
- Disk interface MUST be `NVMe` (installed Windows bound inbox `stornvme`; VirtIO interface will not boot).
- Acceleration MUST be `-accel hvf -cpu max` — do NOT carry `pauth-impdef=on` (TCG-only; errors under HVF).
- The VM is headless-only, forever — never launched as a desktop window.
- `answer.iso` stays attached (NetKVM + utm-guest-tools fallback); the Windows install ISO is detached.
- macOS file-lock rule: any attached `.dmg`/disk image must be `hdiutil detach`ed before QEMU launch.
- Port forwards: host `2222` → guest 22 (SSH), host `3389` → guest 3389 (RDP).
- All VM-side commands typed via HMP `sendkey` using `/tmp/type.sh`; screens via `screendump` + `sips` + `swift /tmp/ocr.swift`.

---

### Task 1: Shut down the TCG VM cleanly

**Files:**
- None created/modified (ops-only).

**Interfaces:**
- Consumes: running QEMU TCG VM, HMP socket `/tmp/w11tcg-mon.sock`.
- Produces: an unlocked, consistent `win11.qcow2`; freed host resources.

- [ ] **Step 1: Confirm the TCG VM is still alive**

Run: `ps -o pid,etime,%cpu,stat -p 50552`
Expected: a running `qemu-system-aarch64` process. If the pid is gone, skip the remaining steps (VM already down).

- [ ] **Step 2: Send ACPI powerdown and wait for exit**

```bash
(printf 'system_powerdown\n'; sleep 2) | nc -U /tmp/w11tcg-mon.sock >/dev/null 2>&1
for i in $(seq 1 30); do ps -p 50552 >/dev/null 2>&1 || break; sleep 5; done
ps -p 50552 >/dev/null 2>&1 && echo "STILL RUNNING" || echo "VM EXITED"
```
Expected: `VM EXITED` within ~150 s. If still running after the loop, force `quit` on the monitor and re-check.

- [ ] **Step 3: Verify the qcow2 is not locked and note its size**

Run: `ls -la /Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2`
Expected: file present, size ≈ 19 GB (19244253184 bytes), no QEMU process holding it.

- [ ] **Step 4: Commit (documentation placeholder)**

```bash
cd /Users/wayne/git/ghost && git add -A && git commit -q -m "ops: shut down TCG Windows VM" 2>/dev/null || echo "nothing to commit"
```
Expected: commit succeeds or reports nothing to commit (acceptable — this is an ops task).

---

### Task 2: Create the UTM-native bundle

**Files:**
- Create: `/Users/wayne/vms/ghost-win11-utm.utm/config.plist`
- Create: `/Users/wayne/vms/ghost-win11-utm.utm/Data/` (directory; qcow2 lives in the ORIGINAL bundle — do not move it)

**Interfaces:**
- Consumes: existing qcow2 at `/Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2`; `answer.iso` at `/Users/wayne/vms/qemu-win11/answer.iso`.
- Produces: `config.plist` describing an HVF/NVMe/headless-compatible VM (used directly by Task 3's launch; the plist documents the exact topology).

- [ ] **Step 1: Create the bundle directories**

```bash
mkdir -p /Users/wayne/vms/ghost-win11-utm.utm/Data
ls -la /Users/wayne/vms/ghost-win11-utm.utm
```
Expected: directory tree exists.

- [ ] **Step 2: Write the config.plist**

Write `/Users/wayne/vms/ghost-win11-utm.utm/config.plist` with this exact content (UUIDs below are new; replace if colliding):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Backend</key>
	<string>QEMU</string>
	<key>ConfigurationVersion</key>
	<integer>4</integer>
	<key>Information</key>
	<dict>
		<key>Name</key>
		<string>ghost-win11-utm</string>
		<key>Icon</key>
		<string>windows-11</string>
		<key>IconCustom</key>
		<false/>
		<key>Notes</key>
		<string>Windows 11 25H2 ARM64, HVF accel, NVMe, headless agent-test VM</string>
		<key>UUID</key>
		<string>8F3A6D14-1C9E-4A71-9D4B-2E77C5A9F0A1</string>
	</dict>
	<key>System</key>
	<dict>
		<key>Architecture</key>
		<string>aarch64</string>
		<key>CPU</key>
		<string>max</string>
		<key>CPUCount</key>
		<integer>8</integer>
		<key>CPUFlagsAdd</key>
		<array/>
		<key>CPUFlagsRemove</key>
		<array/>
		<key>ForceMulticore</key>
		<true/>
		<key>JITCacheSize</key>
		<integer>0</integer>
		<key>MemorySize</key>
		<integer>8192</integer>
		<key>Target</key>
		<string>virt</string>
	</dict>
	<key>QEMU</key>
	<dict>
		<key>AdditionalArguments</key>
		<array/>
		<key>BalloonDevice</key>
		<false/>
		<key>DebugLog</key>
		<false/>
		<key>Hypervisor</key>
		<true/>
		<key>PS2Controller</key>
		<false/>
		<key>RNGDevice</key>
		<true/>
		<key>RTCLocalTime</key>
		<true/>
		<key>TPMDevice</key>
		<false/>
		<key>UEFIBoot</key>
		<true/>
	</dict>
	<key>Input</key>
	<dict>
		<key>MaximumUsbShare</key>
		<integer>3</integer>
		<key>UsbBusSupport</key>
		<string>3.0</string>
		<key>UsbSharing</key>
		<false/>
	</dict>
	<key>Sharing</key>
	<dict>
		<key>ClipboardSharing</key>
		<false/>
		<key>DirectoryShareMode</key>
		<string>None</string>
		<key>DirectoryShareReadOnly</key>
		<false/>
	</dict>
	<key>Display</key>
	<array>
		<dict>
			<key>DynamicResolution</key>
			<false/>
			<key>DownscalingFilter</key>
			<string>Linear</string>
			<key>Hardware</key>
			<string>virtio-ramfb</string>
			<key>NativeResolution</key>
			<false/>
			<key>UpscalingFilter</key>
			<string>Nearest</string>
		</dict>
	</array>
	<key>Drive</key>
	<array>
		<dict>
			<key>ImageName</key>
			<string>win11.qcow2</string>
			<key>ImageType</key>
			<string>Disk</string>
			<key>Interface</key>
			<string>NVMe</string>
			<key>InterfaceVersion</key>
			<integer>1</integer>
			<key>Identifier</key>
			<string>B1B4C0DE-0000-4000-8000-000000000001</string>
			<key>ReadOnly</key>
			<false/>
		</dict>
		<dict>
			<key>ImageName</key>
			<string>answer.iso</string>
			<key>ImageType</key>
			<string>CD</string>
			<key>Interface</key>
			<string>USB</string>
			<key>InterfaceVersion</key>
			<integer>1</integer>
			<key>Identifier</key>
			<string>B1B4C0DE-0000-4000-8000-000000000002</string>
			<key>ReadOnly</key>
			<true/>
		</dict>
	</array>
	<key>Network</key>
	<array>
		<dict>
			<key>Hardware</key>
			<string>virtio-net-pci</string>
			<key>IsolateFromHost</key>
			<false/>
			<key>MacAddress</key>
			<string>b6:7a:90:ef:99:c1</string>
			<key>Mode</key>
			<string>Emulated</string>
			<key>PortForward</key>
			<array>
				<dict>
					<key>GuestPort</key>
					<integer>22</integer>
					<key>HostPort</key>
					<integer>2222</integer>
					<key>Protocol</key>
					<string>TCP</string>
				</dict>
				<dict>
					<key>GuestPort</key>
					<integer>3389</integer>
					<key>HostPort</key>
					<integer>3389</integer>
					<key>Protocol</key>
					<string>TCP</string>
				</dict>
			</array>
		</dict>
	</array>
	<key>Serial</key>
	<array/>
	<key>Sound</key>
	<array/>
</dict>
</plist>
```

- [ ] **Step 3: Validate the plist parses**

Run: `plutil -lint /Users/wayne/vms/ghost-win11-utm.utm/config.plist`
Expected: `OK`. If it reports a missing file reference note, that is expected — the qcow2 lives in the original bundle and QEMU will be pointed at it explicitly in Task 3.

- [ ] **Step 4: Copy answer.iso into the new bundle's Data dir**

```bash
cp /Users/wayne/vms/qemu-win11/answer.iso /Users/wayne/vms/ghost-win11-utm.utm/Data/answer.iso
ls -la /Users/wayne/vms/ghost-win11-utm.utm/Data/
```
Expected: `answer.iso` present (82,276,352 bytes). (Keep the original copy too — the launch command will reference the bundle copy.)

- [ ] **Step 5: Commit**

```bash
cd /Users/wayne/git/ghost && git add -A && git commit -q -m "chore: scaffold UTM-native ghost-win11-utm bundle" 2>/dev/null || echo "nothing to commit"
```

---

### Task 3: Launch the VM headless with HVF

**Files:**
- None created/modified in the repo (ops-only). Logs land in `/tmp/w11utm/`.

**Interfaces:**
- Consumes: `config.plist` topology from Task 2; qcow2; `answer.iso` bundle copy.
- Produces: running HVF VM with HMP socket `/tmp/w11utm-mon.sock`; serial log `/tmp/w11utm-serial.log`.

- [ ] **Step 1: Ensure no stale sockets/processes**

```bash
rm -f /tmp/w11utm-mon.sock /tmp/w11utm-serial.log
mkdir -p /tmp/w11utm
lsof /Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2 2>/dev/null | grep qemu || echo "disk not locked"
```
Expected: `disk not locked` (Task 1 shut the TCG VM down).

- [ ] **Step 2: Launch the VM**

```bash
cd /tmp/w11utm && nohup /opt/homebrew/bin/qemu-system-aarch64 \
  -machine virt -m 8G -cpu max -smp 8 \
  -accel hvf \
  -bios /opt/homebrew/share/qemu/edk2-aarch64-code.fd \
  -device ramfb \
  -device qemu-xhci,id=usb1 -device usb-kbd,bus=usb1.0 -device usb-tablet,bus=usb1.0 \
  -device qemu-xhci,id=usb2 \
  -device usb-storage,drive=answer,bus=usb2.0 \
  -drive if=none,id=answer,format=raw,media=cdrom,file=/Users/wayne/vms/ghost-win11-utm.utm/Data/answer.iso \
  -device nvme,drive=system,serial=system,bootindex=0 \
  -drive if=none,id=system,format=qcow2,file=/Users/wayne/vms/ghost-win11.utm/Data/win11.qcow2 \
  -nic user,model=virtio-net-pci,hostfwd=tcp::2222-:22,hostfwd=tcp::3389-:3389 \
  -monitor unix:/tmp/w11utm-mon.sock,server,nowait \
  -serial file:/tmp/w11utm-serial.log \
  -display none > /tmp/w11utm/stdout.log 2>&1 &
echo "launched pid $!"; sleep 8; ps aux | grep qemu-system-aarch64 | grep -v grep | awk '{print $2, $3"%"}'
```
Expected: a qemu pid with high CPU% (HVF shows multiple vCPU threads).

- [ ] **Step 3: Verify the monitor socket responds**

Run: `(printf 'info block\n'; sleep 2) | nc -U /tmp/w11utm-mon.sock | grep -E "system|answer"`
Expected: both `system` (qcow2) and `answer` (cdrom) block devices listed.

- [ ] **Step 4: Verify the guest boots to Windows**

```bash
sleep 60
(printf 'screendump /tmp/w11utm-boot.ppm\n'; sleep 4) | nc -U /tmp/w11utm-mon.sock >/dev/null 2>&1
sips -s format png /tmp/w11utm-boot.ppm --out /tmp/w11utm-boot.png >/dev/null 2>&1
swift /tmp/ocr.swift /tmp/w11utm-boot.png 2>/dev/null
```
Expected: OCR shows the Windows desktop or the "Good things coming your way" first-boot screen — NOT the EFI shell. If the EFI shell appears, the bootindex/NVMe topology is wrong; stop and revisit Task 2 before proceeding.

- [ ] **Step 5: Commit**

```bash
cd /Users/wayne/git/ghost && git add -A && git commit -q -m "ops: launch UTM-native HVF Windows VM headless" 2>/dev/null || echo "nothing to commit"
```

---

### Task 4: Finish the OpenSSH Server install and verify networking

**Files:**
- None in the repo (ops-only; guest-side changes live in the VM disk).

**Interfaces:**
- Consumes: VM from Task 3 at HMP socket `/tmp/w11utm-mon.sock`; `answer.iso` NetKVM fallback driver.
- Produces: working `sshd` in the guest on port 22, reachable at `localhost:2222`.

- [ ] **Step 1: Confirm the FirstLogon PowerShell is done or absent**

```bash
(printf 'screendump /tmp/w11utm-p1.ppm\n'; sleep 4) | nc -U /tmp/w11utm-mon.sock >/dev/null 2>&1
sips -s format png /tmp/w11utm-p1.ppm --out /tmp/w11utm-p1.png >/dev/null 2>&1
swift /tmp/ocr.swift /tmp/w11utm-p1.png 2>/dev/null
```
Expected: desktop, or a PowerShell window. If the PowerShell "Operation Running" window is present, wait (poll every 60 s) until it closes — under HVF it completes in minutes, not the hour TCG took.

- [ ] **Step 2: Test SSH from the host**

Run: `ssh -o StrictHostKeyChecking=no -o ConnectTimeout=12 -o BatchMode=yes ghost@localhost -p 2222 'echo SSH_OK; whoami'`
Expected: `SSH_OK` + `ghost`. If "banner exchange" timeout persists, proceed to Step 3.

- [ ] **Step 3 (fallback): Install OpenSSH via the guest's admin console**

If SSH is not yet up, open an admin PowerShell in the guest via HMP (Win key via `sendkey meta_l` then type `powershell`, `ctrl-shift-enter`, `alt-y` on the UAC prompt — reuse the `/tmp/type.sh` helper), then run:

```
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Set-Service sshd -StartupType Automatic
Start-Service sshd
New-NetFirewallRule -Name sshd -DisplayName 'OpenSSH Server' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
```

- [ ] **Step 4 (fallback): Verify/enable virtio-net**

If sshd is running but `localhost:2222` still won't connect, the virtio-net NIC may not have enumerated. From the guest admin console install NetKVM from the attached `answer.iso`:

```powershell
pnputil /add-driver D:\drivers\NetKVM\netkvm.inf /install
```
(Confirm the CD drive letter with `Get-Volume`; it may not be `D:`.) Then re-test Step 2.

- [ ] **Step 5: Confirm ghost mcp init prerequisites**

Run: `ssh -o StrictHostKeyChecking=no ghost@localhost -p 2222 'powershell -Command "Get-Service sshd | Select Status; Get-NetFirewallRule -Name sshd | Select Enabled" 2>&1'`
Expected: sshd `Running`, firewall rule `True`.

- [ ] **Step 6: Commit**

```bash
cd /Users/wayne/git/ghost && git add -A && git commit -q -m "ops: OpenSSH Server online in UTM Windows VM" 2>/dev/null || echo "nothing to commit"
```

---

### Task 5: Confirm end-to-end ghost access

**Files:**
- None in the repo (verification only).

**Interfaces:**
- Consumes: SSH at `ghost@localhost:2222` from Task 4.
- Produces: confirmation that the original testing goal (run ghost inside Windows) is reachable.

- [ ] **Step 1: SSH banner and ghost binary check**

Run:
```bash
ssh -o StrictHostKeyChecking=no ghost@localhost -p 2222 'echo SSH_OK; whoami; where ghost 2>NUL || echo NO_GHOST'
```
Expected: `SSH_OK`, `ghost`, and either a path or `NO_GHOST` (ghost binary is out of scope for this plan; the VM being reachable is the deliverable).

- [ ] **Step 2: Leave the VM running and note how to manage it**

Run: `ps -o pid,etime,%cpu -C qemu-system-aarch64 | head -3`
Expected: pid line. Record for handoff: HMP socket `/tmp/w11utm-mon.sock`; stop via `(printf 'system_powerdown\n'; sleep 2) | nc -U /tmp/w11utm-mon.sock`.

- [ ] **Step 3: Commit final state**

```bash
cd /Users/wayne/git/ghost && git add -A && git commit -q -m "docs: UTM Windows VM verified reachable over SSH" 2>/dev/null || echo "nothing to commit"
```
