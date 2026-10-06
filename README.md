# beszel-export

Exports the systems monitored by a [Beszel](https://github.com/henrygd/beszel) hub to a Markdown inventory table, followed by a fleet summary.

It reads the hub's PocketBase SQLite database directly in read-only mode, so the hub does not need to be running and nothing is written to the database.

## Download

Prebuilt binaries for Windows x64 and Linux x64 are attached to each release on the [Releases page](https://github.com/xavier-hernandez/beszel-exporter/releases/latest):

| Platform | File |
| --- | --- |
| Windows x64 | `beszel-export-windows-amd64.exe` |
| Linux x64 | `beszel-export-linux-amd64` |

The Linux binary is statically linked and runs on any x64 distribution. After downloading it, make it executable with `chmod +x beszel-export-linux-amd64`.

The [build workflow](.github/workflows/build.yml) builds both binaries on every push to `main` and on pull requests. You can download them from the workflow run's **Artifacts** on the [Actions page](https://github.com/xavier-hernandez/beszel-exporter/actions).

To publish a release, push a tag that starts with `v`. The workflow then attaches both binaries to a new GitHub Release:

```sh
git tag v1.0.0
git push origin v1.0.0
```

## Build from source

Requires Go 1.27+. The SQLite driver is pure Go (`modernc.org/sqlite`), so no CGO or C compiler is needed.

```sh
git clone https://github.com/xavier-hernandez/beszel-exporter.git
cd beszel-exporter
go build -o beszel-export.exe .   # Windows
go build -o beszel-export .       # Linux
```

## Usage

```sh
beszel-export -db ./beszel_data/data.db -out systems.md [-location]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-db` | `beszel_data/data.db` | Path to the hub's `data.db` |
| `-out` | stdout | Output Markdown file |
| `-location` | off | Look up each host's location from its IP address and add a **Location** column |

Examples:

```sh
# Print to the terminal
beszel-export -db ./beszel_data/data.db

# Write a file with locations
beszel-export -db ./beszel_data/data.db -out systems.md -location
```

## Output

### Systems table

There is one row per system, sorted by name:

| Column | Source |
| --- | --- |
| System | Name given in the hub |
| Hostname | Agent-reported hostname |
| Host / IP | Address the hub connects to |
| Location | City, state and country (only with `-location`) |
| Port | Agent port |
| OS, Kernel | Agent-reported OS and kernel version |
| Memory | Total RAM |
| CPU, Cores | CPU model, cores (and threads when they differ) |
| Disks | Physical drives / RAID arrays from SMART data, otherwise filesystem sizes |
| Disk Used | Last reported root filesystem usage |

A cell shows `—` when the agent never reported that value.

### Summary table

This table is added at the end:

| Metric | Meaning |
| --- | --- |
| Systems | Number of systems in the hub |
| Total RAM | Best-guess total memory |
| Total disk space | Best-guess total storage capacity |
| Total CPU cores | Best-guess total cores (and threads) |

The totals are **estimates**. Each one is the sum of the values that systems reported. Every system that reported nothing is counted at the **median** of the reported values. The median keeps a few large dedicated servers from inflating the guess. The **Basis** column shows the breakdown, for example: `8.27 TB reported by 41 systems + 23 unreported at median 24.5 GB`.

## Where the data comes from

Different Beszel agent versions store different data, so the tool checks several sources and uses the best one available:

1. **`systems.info`**: summary JSON written by all agents. It gives the hostname, OS, kernel, CPU, cores and disk usage, and is the fallback for older agents.
2. **`system_details`**: written by newer agents. It overrides hostname, OS, kernel, CPU, cores, threads and memory.
3. **`smart_devices`**: physical drives and RAID arrays, if the agent collects SMART data.
4. **`system_stats`** (latest record): filesystem sizes when there is no SMART data, and memory when `system_details` is missing.

For the disk total, filesystem capacity from `system_stats` is preferred. SMART data lists RAID member drives as well as their arrays, so it would count them twice. When only SMART data is available, the arrays are counted but their member drives are not.

## Location lookup

With `-location`, each host is resolved to an IP address. Each unique IP is then sent to `https://xapi.xavier.cc/Ip/<ip>`, with at most 8 requests at a time and a 10-second timeout. If a lookup fails, the tool logs it to stderr and leaves the cell empty. The rest of the export still completes.

Note that this sends every host's IP address to that service.

## Files

| File | Purpose |
| --- | --- |
| `main.go` | Flags, database queries, Markdown and summary output |
| `geo.go` | IP resolution and location lookup |

## License

MIT. See [LICENSE](LICENSE).
