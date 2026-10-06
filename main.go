// Command beszel-export reads a Beszel hub database (PocketBase SQLite) and writes
// a Markdown table of the monitored systems: name, hostname, host/IP, agent port,
// OS, kernel, memory, CPU, disks and root disk usage.
//
// Usage:
//
//	beszel-export -db ./beszel_data/data.db -out systems.md [-location]
//
// With -location, each host's IP is looked up via the xapi.xavier.cc IP API
// and a Location column (city, state, country) is added.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type system struct {
	ID       string
	Name     string
	Hostname string
	Host     string
	Location string
	Port     string
	OS       string
	Kernel   string
	Memory   uint64 // bytes
	CPU      string
	Cores    int
	Threads  int
	Disks    []string
	DiskPct  float64 // root filesystem usage, last reported

	// Best-guess totals for the summary table.
	DiskBytes uint64 // total storage capacity
	MemGuess  bool   // Memory came from the latest stats record, not system_details
}

func main() {
	dbPath := flag.String("db", "beszel_data/data.db", "path to the Beszel hub data.db")
	outPath := flag.String("out", "", "output Markdown file (default: stdout)")
	withLocation := flag.Bool("location", false, "look up each host's location from its IP address")
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("database not found: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	systems, err := loadSystems(db)
	if err != nil {
		log.Fatal(err)
	}
	if *withLocation {
		fillLocations(systems)
	}

	var w io.Writer = os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}
	writeMarkdown(w, systems, *withLocation)
}

func loadSystems(db *sql.DB) ([]*system, error) {
	rows, err := db.Query(`SELECT id, name, host, port, info FROM systems ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("query systems: %w", err)
	}
	defer rows.Close()

	var systems []*system
	byID := map[string]*system{}
	for rows.Next() {
		s := &system{}
		var port, info sql.NullString
		if err := rows.Scan(&s.ID, &s.Name, &s.Host, &port, &info); err != nil {
			return nil, err
		}
		s.Port = port.String
		applyInfoFallback(s, info.String)
		systems = append(systems, s)
		byID[s.ID] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hardware details (newer agents). Optional table, so errors are not fatal.
	if rows, err := db.Query(`SELECT system, hostname, os_name, kernel, cpu, cores, threads, memory FROM system_details`); err == nil {
		for rows.Next() {
			var id string
			var hostname, osName, kernel, cpu sql.NullString
			var cores, threads sql.NullInt64
			var mem sql.NullFloat64
			if rows.Scan(&id, &hostname, &osName, &kernel, &cpu, &cores, &threads, &mem) != nil {
				continue
			}
			s := byID[id]
			if s == nil {
				continue
			}
			if hostname.String != "" {
				s.Hostname = hostname.String
			}
			if osName.String != "" {
				s.OS = osName.String
			}
			if kernel.String != "" {
				s.Kernel = kernel.String
			}
			if cpu.String != "" {
				s.CPU = cpu.String
			}
			if cores.Int64 > 0 {
				s.Cores = int(cores.Int64)
			}
			if threads.Int64 > 0 {
				s.Threads = int(threads.Int64)
			}
			if mem.Float64 > 0 {
				s.Memory = uint64(mem.Float64)
			}
		}
		rows.Close()
	}

	// Physical drives from SMART data, if the agent collects it.
	smart := map[*system][]smartDevice{}
	if rows, err := db.Query(`SELECT system, name, model, type, capacity FROM smart_devices ORDER BY name`); err == nil {
		for rows.Next() {
			var id string
			var name, model, devType sql.NullString
			var capacity sql.NullFloat64
			if rows.Scan(&id, &name, &model, &devType, &capacity) != nil || capacity.Float64 <= 0 {
				continue
			}
			if s := byID[id]; s != nil {
				s.Disks = append(s.Disks, deviceLabel(name.String, model.String, devType.String)+" "+formatBytes(uint64(capacity.Float64)))
				smart[s] = append(smart[s], smartDevice{devType.String == "mdraid", uint64(capacity.Float64)})
			}
		}
		rows.Close()
	}

	// The most recent stats record fills gaps: filesystem sizes when there is
	// no SMART data, memory when system_details is missing, and disk capacity.
	for _, s := range systems {
		st := latestStats(db, s.ID)
		if len(s.Disks) == 0 {
			s.Disks = st.filesystems()
		}
		if s.Memory == 0 && st.MemTotal > 0 {
			s.Memory = uint64(st.MemTotal * (1 << 30))
			s.MemGuess = true
		}
		// Prefer filesystem capacity: SMART lists RAID members alongside
		// their arrays, which would double count.
		if s.DiskBytes = st.diskBytes(); s.DiskBytes == 0 {
			s.DiskBytes = smartCapacity(smart[s])
		}
	}
	return systems, nil
}

type smartDevice struct {
	raid     bool
	capacity uint64
}

// smartCapacity totals SMART devices, counting only arrays when there are any
// so that RAID member drives are not added twice.
func smartCapacity(devs []smartDevice) uint64 {
	hasRaid := slices.ContainsFunc(devs, func(d smartDevice) bool { return d.raid })
	var total uint64
	for _, d := range devs {
		if d.raid == hasRaid {
			total += d.capacity
		}
	}
	return total
}

// deviceLabel names a SMART device: RAID arrays by device and level
// (e.g. "md3 RAID1"), physical drives by model.
func deviceLabel(name, model, devType string) string {
	dev := strings.TrimPrefix(name, "/dev/")
	if devType == "mdraid" {
		level := ""
		if i, j := strings.Index(model, "("), strings.Index(model, ")"); i >= 0 && j > i {
			level = " " + strings.ToUpper(model[i+1:j])
		}
		return dev + level
	}
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	return dev
}

// osNames maps the numeric os value agents report (system.Os) to a name.
var osNames = map[int]string{0: "Linux", 1: "macOS", 2: "Windows", 3: "FreeBSD"}

// applyInfoFallback fills hostname, OS, kernel and CPU fields from the
// systems.info JSON, which older agents populated before system_details
// existed. It also reads the last reported root disk usage.
func applyInfoFallback(s *system, raw string) {
	if raw == "" {
		return
	}
	var info struct {
		Hostname string  `json:"h"`
		Kernel   string  `json:"k"`
		Cores    int     `json:"c"`
		Threads  int     `json:"t"`
		Model    string  `json:"m"`
		OS       *int    `json:"os"`
		DiskPct  float64 `json:"dp"`
	}
	if json.Unmarshal([]byte(raw), &info) != nil {
		return
	}
	s.Hostname, s.Cores, s.Threads, s.CPU = info.Hostname, info.Cores, info.Threads, info.Model
	s.DiskPct = info.DiskPct
	kernel := strings.TrimSpace(info.Kernel)
	switch {
	case info.OS == nil:
		s.Kernel = kernel
	case *info.OS == 2 && kernel != "":
		s.OS = kernel // Windows agents report the edition name in the kernel field
	default:
		s.OS, s.Kernel = osNames[*info.OS], kernel
	}
}

type stats struct {
	MemTotal  float64 `json:"m"` // GiB
	DiskTotal float64 `json:"d"` // GiB
	ExtraFs   map[string]struct {
		DiskTotal float64 `json:"d"` // GiB
	} `json:"efs"`
}

// latestStats returns the most recent stats record, or zero values if none.
func latestStats(db *sql.DB, systemID string) stats {
	var st stats
	var raw string
	if db.QueryRow(`SELECT stats FROM system_stats WHERE system = ? ORDER BY created DESC LIMIT 1`, systemID).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &st)
	}
	return st
}

func (st stats) filesystems() []string {
	var disks []string
	if st.DiskTotal > 0 {
		disks = append(disks, "root "+formatGiB(st.DiskTotal))
	}
	names := make([]string, 0, len(st.ExtraFs))
	for name := range st.ExtraFs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if d := st.ExtraFs[name].DiskTotal; d > 0 {
			disks = append(disks, name+" "+formatGiB(d))
		}
	}
	return disks
}

func (st stats) diskBytes() uint64 {
	gb := st.DiskTotal
	for _, fs := range st.ExtraFs {
		gb += fs.DiskTotal
	}
	return uint64(gb * (1 << 30))
}

func formatBytes(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MB", b>>20)
	}
	return formatGiB(float64(b) / (1 << 30))
}

func formatGiB(gb float64) string {
	if gb >= 1024 {
		return fmt.Sprintf("%.2f TB", gb/1024)
	}
	return fmt.Sprintf("%.1f GB", gb)
}

func writeMarkdown(w io.Writer, systems []*system, withLocation bool) {
	header := []string{"System", "Hostname", "Host / IP", "Port", "OS", "Kernel", "Memory", "CPU", "Cores", "Disks", "Disk Used"}
	if withLocation {
		header = slices.Insert(header, 3, "Location")
	}
	fmt.Fprintf(w, "# Beszel Systems\n\n_Generated %s_\n\n", time.Now().Format("2006-01-02 15:04"))
	writeRow(w, header)
	writeRow(w, slices.Repeat([]string{"---"}, len(header)))

	for _, s := range systems {
		mem := ""
		if s.Memory > 0 {
			mem = formatBytes(s.Memory)
		}
		cores := ""
		if s.Cores > 0 {
			cores = fmt.Sprint(s.Cores)
			if s.Threads > s.Cores {
				cores += fmt.Sprintf(" (%d threads)", s.Threads)
			}
		}
		diskPct := ""
		if s.DiskPct > 0 {
			diskPct = fmt.Sprintf("%.0f%%", s.DiskPct)
		}
		row := []string{s.Name, s.Hostname, s.Host, s.Port, s.OS, s.Kernel, mem, s.CPU, cores, strings.Join(s.Disks, "<br>"), diskPct}
		if withLocation {
			row = slices.Insert(row, 3, s.Location)
		}
		for i, v := range row {
			row[i] = cell(v)
		}
		writeRow(w, row)
	}
	writeSummary(w, systems)
}

// writeSummary appends fleet totals. Each total is a best guess: the sum of
// reported values, plus the median reported value for every system that never
// reported one (the median resists skew from a few large dedicated servers).
func writeSummary(w io.Writer, systems []*system) {
	var mem, disk, cores, threads []uint64
	memGuessed := 0
	for _, s := range systems {
		if s.Memory > 0 {
			mem = append(mem, s.Memory)
			if s.MemGuess {
				memGuessed++
			}
		}
		if s.DiskBytes > 0 {
			disk = append(disk, s.DiskBytes)
		}
		if s.Cores > 0 {
			cores = append(cores, uint64(s.Cores))
			threads = append(threads, uint64(max(s.Threads, s.Cores)))
		}
	}
	n := len(systems)

	// estimate returns the best-guess total and describes how it was made.
	estimate := func(vals []uint64, format func(uint64) string) (uint64, string) {
		var sum uint64
		for _, v := range vals {
			sum += v
		}
		missing := n - len(vals)
		if missing == 0 || len(vals) == 0 {
			return sum, fmt.Sprintf("%d of %d systems reported", len(vals), n)
		}
		sorted := slices.Clone(vals)
		slices.Sort(sorted)
		median := sorted[len(sorted)/2]
		return sum + median*uint64(missing), fmt.Sprintf("%s reported by %d systems + %d unreported at median %s",
			format(sum), len(vals), missing, format(median))
	}

	count := func(v uint64) string { return fmt.Sprint(v) }
	memTotal, memNote := estimate(mem, formatBytes)
	if memGuessed > 0 {
		memNote += fmt.Sprintf("; %d from latest stats", memGuessed)
	}
	diskTotal, diskNote := estimate(disk, formatBytes)
	coreTotal, cpuNote := estimate(cores, count)
	threadTotal, _ := estimate(threads, count)
	cpu := fmt.Sprint(coreTotal)
	if threadTotal > coreTotal {
		cpu += fmt.Sprintf(" (%d threads)", threadTotal)
	}

	fmt.Fprint(w, "\n## Summary\n\n")
	writeRow(w, []string{"Metric", "Best guess", "Basis"})
	writeRow(w, []string{"---", "---", "---"})
	writeRow(w, []string{"Systems", fmt.Sprint(n), "—"})
	writeRow(w, []string{"Total RAM", "~" + formatBytes(memTotal), memNote})
	writeRow(w, []string{"Total disk space", "~" + formatBytes(diskTotal), diskNote})
	writeRow(w, []string{"Total CPU cores", "~" + cpu, cpuNote})
}

func writeRow(w io.Writer, cells []string) {
	fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
}

// cell escapes pipes so values cannot break the table layout.
func cell(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "—"
	}
	return strings.ReplaceAll(v, "|", `\|`)
}
