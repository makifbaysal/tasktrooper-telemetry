package infra

import (
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestDiskCandidate(t *testing.T) {
	tests := []struct {
		name string
		p    disk.PartitionStat
		want bool
	}{
		{"ext4 root", disk.PartitionStat{Device: "/dev/sda1", Mountpoint: "/", Fstype: "ext4", Opts: []string{"rw", "relatime"}}, true},
		{"xfs data", disk.PartitionStat{Device: "/dev/nvme0n1p2", Mountpoint: "/data", Fstype: "xfs", Opts: []string{"rw"}}, true},
		{"docker desktop data disk", disk.PartitionStat{Device: "/dev/vda1", Mountpoint: "/var/lib/docker", Fstype: "ext4", Opts: []string{"rw"}}, true},
		// Docker Desktop VM: the LinuxKit root image is erofs, 100% used by design
		{"erofs image", disk.PartitionStat{Device: "/dev/root", Mountpoint: "/oldroot", Fstype: "erofs", Opts: []string{"ro"}}, false},
		{"read-only ext4", disk.PartitionStat{Device: "/dev/sdb1", Mountpoint: "/mnt/backup", Fstype: "ext4", Opts: []string{"ro", "noatime"}}, false},
		{"squashfs snap", disk.PartitionStat{Device: "/dev/loop3", Mountpoint: "/snap/core/1", Fstype: "squashfs", Opts: []string{"ro"}}, false},
		{"macOS share", disk.PartitionStat{Device: "/run/host_mark/Users", Mountpoint: "/host_mnt/Users", Fstype: "fakeowner", Opts: []string{"rw"}}, false},
		{"virtiofs share", disk.PartitionStat{Device: "ROSETTA", Mountpoint: "/mnt/rosetta", Fstype: "virtiofs", Opts: []string{"rw"}}, false},
		{"tmpfs", disk.PartitionStat{Device: "tmpfs", Mountpoint: "/tmp", Fstype: "tmpfs", Opts: []string{"rw"}}, false},
		{"container layer", disk.PartitionStat{Device: "overlay", Mountpoint: "/var/lib/docker/overlay2/x/merged", Fstype: "overlay", Opts: []string{"rw"}}, false},
		{"kubelet volume", disk.PartitionStat{Device: "/dev/sdc", Mountpoint: "/var/lib/kubelet/pods/x/volumes/y", Fstype: "ext4", Opts: []string{"rw"}}, false},
		{"run mount", disk.PartitionStat{Device: "/dev/sdd", Mountpoint: "/run/media/usb", Fstype: "ext4", Opts: []string{"rw"}}, false},
		{"efi", disk.PartitionStat{Device: "/dev/sda15", Mountpoint: "/boot/efi", Fstype: "vfat", Opts: []string{"rw"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diskCandidate(tt.p); got != tt.want {
				t.Errorf("diskCandidate(%s on %s) = %v, want %v", tt.p.Fstype, tt.p.Mountpoint, got, tt.want)
			}
		})
	}
}
