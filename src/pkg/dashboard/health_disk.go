package dashboard

import (
	"fmt"
	"syscall"
)

// Early warning for a filling /data volume (#9869).
//
// A spoke whose /data volume (or the node disk behind a local-path PVC) fills
// up dies without warning: kubelet evicts the pod for DiskPressure seconds
// after every start and the ReplicaSet recreates it forever. Nothing in the
// hive degraded first, so the operator's first signal was a crash loop. The
// system gauges already sample /data via Statfs for the dashboard; this file
// turns the same sample into a health check so that the hub alert surface
// (failingHealthChecks reads every non-passing check in HealthSummary) and
// /api/health/deep both flag a filling volume long before eviction.

// Thresholds are percentages of the /data filesystem that is in use.
//
//   - dataDiskWarnPct     ⇒ warn: the volume is filling; plan cleanup/expansion.
//   - dataDiskHighPct     ⇒ warn, stronger wording: act before eviction.
//   - dataDiskCriticalPct ⇒ fail: the hive is one burst of writes away from
//     kubelet eviction. Fail (not warn) so the summary turns "degraded" and
//     the hub raises its standing alert for the spoke.
const (
	dataDiskWarnPct     = 75.0
	dataDiskHighPct     = 85.0
	dataDiskCriticalPct = 95.0
)

// healthCheckDataDisk is the check name both health surfaces use.
const healthCheckDataDisk = "data_disk"

// dataDiskUsage is a point-in-time Statfs sample of the hive data volume.
type dataDiskUsage struct {
	// Path is the mount point that was sampled: dataVolumePath, or rootFSPath
	// when /data reported an implausible size (see sampleDataDiskUsage).
	Path       string
	TotalBytes uint64
	UsedBytes  uint64
}

// Pct returns the used fraction as a percentage, 0 when the total is unknown.
func (u dataDiskUsage) Pct() float64 {
	if u.TotalBytes == 0 {
		return 0
	}
	return float64(u.UsedBytes) / float64(u.TotalBytes) * pctMultiplierSysRes
}

// dataDiskUsageFn samples the data volume for the health checks. A
// package-level seam (mirroring healthAgentStatuses): the real sampler reads
// the host's actual /data, which on a live hive or a saturated CI runner can
// legitimately sit above a threshold and would make every HealthSummary
// assertion in the suite environment-dependent. TestMain pins it to a healthy
// fixture; tests that exercise the thresholds override it and restore nil.
// nil ⇒ sampleDataDiskUsage.
var dataDiskUsageFn func() (dataDiskUsage, bool)

func currentDataDiskUsage() (dataDiskUsage, bool) {
	if dataDiskUsageFn != nil {
		return dataDiskUsageFn()
	}
	return sampleDataDiskUsage()
}

// sampleDataDiskUsage reads the data volume via Statfs. ok=false when the
// mount cannot be read (no /data on this host, e.g. a developer laptop), in
// which case the check is omitted rather than reported as 0% — a missing
// sample is not a healthy disk.
//
// Shares the OCI-NFS workaround with the system gauges: some network mounts
// report ~8 EiB, which would pin the percentage at 0 forever, so an
// implausible total falls back to the root filesystem the pod actually
// lands on.
func sampleDataDiskUsage() (dataDiskUsage, bool) {
	var stat syscall.Statfs_t
	path := dataVolumePath
	if err := syscall.Statfs(path, &stat); err != nil {
		return dataDiskUsage{}, false
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	if totalBytes > maxReasonableDiskBytes {
		path = rootFSPath
		if err := syscall.Statfs(path, &stat); err != nil {
			return dataDiskUsage{}, false
		}
		totalBytes = stat.Blocks * uint64(stat.Bsize)
	}
	if totalBytes == 0 {
		return dataDiskUsage{}, false
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	if freeBytes > totalBytes {
		freeBytes = totalBytes
	}
	return dataDiskUsage{Path: path, TotalBytes: totalBytes, UsedBytes: totalBytes - freeBytes}, true
}

// dataDiskHealth classifies a sample into the health-check vocabulary
// (pass/warn/fail) with an operator-facing detail that names the mount, the
// percentage and the free space, so the hub tooltip answers "which disk, how
// bad, how much room is left" without a kubectl exec.
func dataDiskHealth(u dataDiskUsage) (status, detail string) {
	pct := u.Pct()
	freeGB := float64(u.TotalBytes-u.UsedBytes) / bytesPerGB
	totalGB := float64(u.TotalBytes) / bytesPerGB
	where := fmt.Sprintf("%s %.0f%% used (%.1f GB free of %.1f GB)", u.Path, pct, freeGB, totalGB)
	switch {
	case pct >= dataDiskCriticalPct:
		return "fail", where + " — critically full; kubelet DiskPressure eviction is imminent, free space or expand the volume now"
	case pct >= dataDiskHighPct:
		return "warn", where + " — high; clean up agent homes/caches or expand the volume before the pod is evicted"
	case pct >= dataDiskWarnPct:
		return "warn", where + " — filling; plan cleanup or expansion"
	}
	return "pass", where
}
