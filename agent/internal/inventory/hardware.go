// Hardware inventory collector — CPU model, RAM, disk geometry/serials.
// Uses gopsutil for CPU/RAM (no CGO) and platform-specific disk serial probes.
package inventory

import (
	"context"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// HardwareInfo contains the collected hardware inventory.
type HardwareInfo struct {
	CPUModel   string
	CPUVendor  string
	CPUCores   uint32
	CPULogical uint32
	RAMTotal   uint64
	RAMModel   string // best-effort (DMSP on Windows, /sys on Linux)
	OSName     string
	OSVersion  string
	OSArch     string
	Hostname   string
	Domains    []DomainInfo
	Disks      []DiskInfo
}

// DomainInfo describes AD/MDM affiliation.
type DomainInfo struct {
	Type      string // "active_directory" | "apple_business_manager" | "local"
	Name      string
	Workgroup string
}

// DiskInfo describes a physical disk's geometry and serial.
type DiskInfo struct {
	Device string
	Model  string
	Serial string
	Size   uint64
	Type   string // "hdd" | "ssd" | "unknown"
}

// CollectHardware gathers hardware inventory for this host.
func CollectHardware(ctx context.Context) (*HardwareInfo, error) {
	info := &HardwareInfo{}

	// CPU info
	if cpuInfo, err := cpu.InfoWithContext(ctx); err == nil && len(cpuInfo) > 0 {
		info.CPUModel = cpuInfo[0].ModelName
		info.CPUVendor = cpuInfo[0].VendorID
	}
	if count, err := cpu.CountsWithContext(ctx, true); err == nil {
		info.CPULogical = uint32(count)
	}
	if count, err := cpu.CountsWithContext(ctx, false); err == nil {
		info.CPUCores = uint32(count)
	}

	// RAM
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		info.RAMTotal = vm.Total
	}

	// OS info
	info.OSName = runtime.GOOS
	if hInfo, err := host.InfoWithContext(ctx); err == nil {
		info.OSName = hInfo.OS
		info.OSVersion = hInfo.KernelVersion
		info.Hostname = hInfo.Hostname
	}
	info.OSArch = runtime.GOARCH

	// Domain membership
	info.Domains = collectDomainMembership(ctx)

	// Disk info
	info.Disks = collectDiskInfo(ctx)

	return info, nil
}

// collectDiskInfo gathers per-disk geometry and serial info.
func collectDiskInfo(ctx context.Context) []DiskInfo {
	var disks []DiskInfo

	// Try platform-specific serial probes
	serials := diskSerials(ctx)

	// Get disk partitions to map devices
	if parts, err := disk.PartitionsWithContext(ctx, false); err == nil {
		seen := make(map[string]bool)
		for _, p := range parts {
			if seen[p.Device] {
				continue
			}
			seen[p.Device] = true

			di := DiskInfo{Device: p.Device}

			// Usage for size
			if usage, err := disk.UsageWithContext(ctx, p.Mountpoint); err == nil && usage != nil {
				di.Size = usage.Total
			}

			// Check serial if available
			if s, ok := serials[p.Device]; ok {
				di.Serial = s
			}

			// Try to determine SSD vs HDD
			di.Type = detectDiskType(p.Device)

			disks = append(disks, di)
		}
	}

	return disks
}

// diskSerials returns device->serial map via platform-specific means.
func diskSerials(ctx context.Context) map[string]string {
	return diskSerialsPlatform(ctx)
}

// toProto converts HardwareInfo to the wire format.
func (h *HardwareInfo) ToProto() *agentv1.HardwareInfo {
	ph := &agentv1.HardwareInfo{
		CpuModel:      h.CPUModel,
		CpuVendor:     h.CPUVendor,
		CpuCores:      h.CPUCores,
		CpuLogical:    h.CPULogical,
		RamTotalBytes: h.RAMTotal,
		RamModel:      h.RAMModel,
		OsName:        h.OSName,
		OsVersion:     h.OSVersion,
		OsArch:        h.OSArch,
		Hostname:      h.Hostname,
	}

	for _, d := range h.Domains {
		ph.Domains = append(ph.Domains, &agentv1.DomainInfo{
			Type:      d.Type,
			Name:      d.Name,
			Workgroup: d.Workgroup,
		})
	}

	for _, d := range h.Disks {
		ph.Disks = append(ph.Disks, &agentv1.DiskInfo{
			Device: d.Device,
			Model:  d.Model,
			Serial: d.Serial,
			Size:   d.Size,
			Type:   d.Type,
		})
	}

	return ph
}
