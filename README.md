# kerneljudge

The Linux kernel, and the filesystem's own native tools, as the judge of a
[go-filesystems](https://github.com/go-filesystems) driver that has no
independent oracle. It runs autonomously in a [Tart](https://tart.run) Linux VM
on a Mac: no root on the host, no user action.

A driver's test calls `kerneljudge.Run` (correctness) and `kerneljudge.RunPerf`
(performance) behind `FS_KERNEL_JUDGE=1`. The `kerneljudge` command builds that
test for linux/arm64, starts the VM, runs it as root there, and stops the VM.

```sh
go install github.com/go-filesystems/kerneljudge/cmd/kerneljudge@latest
tart clone ghcr.io/cirruslabs/ubuntu:latest fsjudge-vm          # once
kerneljudge run --driver ~/clones/btrfs                          # correctness
kerneljudge run --driver ~/clones/btrfs --perf --rounds 7        # performance
```

The VM is Ubuntu 24.04 arm64 (kernel 6.17) with the `admin` user, passwordless
`sudo` and the Tart guest agent. `kerneljudge run` installs what it needs
(`btrfs-progs exfatprogs squashfs-tools dosfstools e2fsprogs python3-olefile
p7zip-full attr`) on first use, needs NAT networking only for that, and stops the
VM afterwards (`--attach` uses a VM you already started and leaves it running;
`--keep` keeps every examined image in `~/kj/images` in the VM so a mismatch
can be reproduced by hand).

## What it checks

The truth is never the driver. It is a deterministic **corpus**: directories
nested several deep, empty, 1-byte, 511, 4096, 4097 and 65537-byte files, a 1 MiB
and a 32 MiB file of pseudo-random bytes, a sparse file (4 KiB of data, then a
hole), symlinks (relative, absolute, dangling, 200-byte target), xattrs
(including an empty value and a 600-byte one), distinct modes and modification
times, and names in UTF-8 (`données/café.txt`, `日本語/ファイル.txt`,
`emoji-📁/😀.bin`, spaces and parentheses). What a driver or a format cannot hold
(exFAT: no symlinks, xattrs or permissions) is left out of the corpus for it, not
silently skipped.

Per image, three judges compare it with the corpus (tree, kind, permission bits,
size, SHA-256 of every file, symlink targets, mtime, `user.*` xattrs):

1. the native checker: `btrfs check --readonly`, `fsck.exfat -n`,
   `unsquashfs -lls` and `-stat`;
2. the kernel: loop-mount the image read-only and walk it;
3. our own reader.

Two directions:

* **write**: our writer makes the image (`Format` and `WriteFile`, or `BuildFromDir`
  for each compressor);
* **reverse**: the kernel (`mkfs`, a read-write loop mount, `PopulateDir`) or the
  native tool (`mksquashfs -comp gzip|lzo|lz4|xz|zstd|none`) writes the image, and
  our reader is held to the same corpus.

A mismatch is reported with the exact path and both values. `Known` entries
(`Driver.Known`) turn a mismatch that already has an issue into a logged
`KNOWN DEFECT` with its issue number, so a driver's test can be green while the
defect stays visible and a new one still fails. When one defect hides the rest
of an image (the kernel refuses the whole filesystem), a `Variant` runs the
writer on a corpus without it. A size-unaligned image the kernel refuses on a
loop device is retried padded, so the rest is still judged.

A format with its own name rules sets `Driver.Map` (applied to every run's
corpus; ISO 9660 level 1 renames to upper-case 8.3 names) and
`Compare.FoldCase` (names compare case-insensitively without a `;1` suffix, as
the Linux iso9660 driver shows them). iso9660 is judged this way: our
`Builder`, `xorriso` and `genisoimage` write, `isoinfo`, the kernel mount and our
reader read.

## Performance, the kernel as the reference

Same image, written by the native tools, in the same VM: a 256 MiB file read
sequentially, and a walk of 4000 small files (`lstat` and read each). The kernel
side mounts the image read-only; our side opens it with the driver
(`OpenFile` + 1 MiB `ReadAt`, `ListDir` + `Stat` + `ReadFile`). Each
measurement includes its own mount or open. Rounds are interleaved
(kernel seq, ours seq, kernel walk, ours walk, repeated; the order inside each
pair alternates every round), the guest page cache is
dropped (`echo 3 > /proc/sys/vm/drop_caches`) before each, the host's 1-minute
load must be at most 8 (`--max-load`) and the VM's at most 1.0, or it refuses.

Measured 2026-11-15, 16-core Apple host, 4-vCPU VM, medians of 7 rounds (btrfs)
and 5 rounds (exfat, squashfs):

| driver | operation | kernel | ours |
|---|---|---:|---:|
| btrfs v0.5.0 | sequential read | 5481 MiB/s | 12389 MiB/s |
| btrfs v0.5.0 | metadata walk | 82447 files/s | 10369 files/s |
| exfat v0.6.0 | sequential read | 5634 MiB/s | 12711 MiB/s |
| exfat v0.6.0 | metadata walk | 13549 files/s | 486 files/s |
| squashfs v0.6.0 | sequential read | 1907 MiB/s | 6511 MiB/s |
| squashfs v0.6.0 | metadata walk | 54134 files/s | 35758 files/s |

Read the sequential rows with care: the VM disk is a file on the host, whose page
cache is not dropped, so both sides read at memory speed through virtio and the
figure compares software paths, not storage. The kernel path goes through the
loop device (a second copy); on btrfs it also verifies data checksums. The walk
rows are where the drivers are slower than the kernel.

## What it found (2026-11-15)

| driver | defects filed |
|---|---|
| btrfs | [#39](https://github.com/go-filesystems/btrfs/issues/39) unaligned file size: the kernel refuses the filesystem; [#40](https://github.com/go-filesystems/btrfs/issues/40) empty file: `bad file extent`; [#41](https://github.com/go-filesystems/btrfs/issues/41) xattr item dir type 0: mount fails, EIO |
| exfat | [#34](https://github.com/go-filesystems/exfat/issues/34) `Format` over 128 MiB; [#35](https://github.com/go-filesystems/exfat/issues/35) directories with length 0: the kernel sees them empty; [#36](https://github.com/go-filesystems/exfat/issues/36) wrong name hash for non-ASCII Latin letters |
| squashfs | [#42](https://github.com/go-filesystems/squashfs/issues/42) image not padded: the kernel refuses it; [#43](https://github.com/go-filesystems/squashfs/issues/43) mtime lost; [#44](https://github.com/go-filesystems/squashfs/issues/44) `Stat` follows symlinks; [#45](https://github.com/go-filesystems/squashfs/issues/45) no `TimesReader`; [#46](https://github.com/go-filesystems/squashfs/issues/46) xz images fail in the kernel |

The reverse direction (kernel and `mksquashfs` write, our reader reads) was
clean for btrfs and exfat, and for squashfs in all six compressors except the
`Stat` and mtime points above.

## Writing a driver's judge test

```go
func TestKernelJudge(t *testing.T) {
	kj.Run(t, kj.Driver{
		Name: "btrfs", FSType: "btrfs",
		Open:   func(img string) (filesystem.Filesystem, error) { return Open(img, -1) },
		Format: func(img string, n int64) (filesystem.Filesystem, error) { return Format(img, n, FormatConfig{}) },
		Mkfs:   []string{"mkfs.btrfs", "-q", "-f", "{img}"},
		Fsck:   [][]string{{"btrfs", "check", "--readonly", "{img}"}},
		Compare: kj.Compare{Mtime: true, Xattrs: true, Perm: true},
	})
}
```

`Builders` and `Natives` cover a writer that builds from a directory
(squashfs); `FSCaps` says what the on-disk format can hold; `Mtime` supplies a
driver-specific time accessor when `filesystem.TimesReader` is not implemented.
Without `FS_KERNEL_JUDGE=1` the test skips and says why; with it, anything but
root on Linux is an error.

Later targets: iso9660, zfs (the VM's kernel has the module), and cfb, which is
not a kernel filesystem and is to be judged by an independent reader such as
`olefile` (installed in the VM).

BSD-3-Clause.
