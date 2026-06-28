import SwiftUI
import AppKit
import Darwin

// Kill a process and all of its descendants (bash -> rsync -> ssh, or ffmpeg).
private func childPids(_ pid: pid_t) -> [pid_t] {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep")
    p.arguments = ["-P", String(pid)]
    let out = Pipe()
    p.standardOutput = out
    p.standardError = Pipe()
    do { try p.run() } catch { return [] }
    let data = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    return (String(data: data, encoding: .utf8) ?? "")
        .split(whereSeparator: { $0.isNewline }).compactMap { pid_t($0) }
}
private func killTree(_ pid: pid_t, _ sig: Int32) {
    for c in childPids(pid) { killTree(c, sig) }
    kill(pid, sig)
}

private struct Job { let route: String?; let args: [String] }

// Runs comma-sync.sh jobs sequentially and streams output. Tracks per-drive state
// so the indexing page can show live progress and survive being closed/reopened.
final class SyncRunner: ObservableObject {
    @Published var log = ""
    @Published var isRunning = false
    @Published var batchTotal = 0
    @Published var batchDone = 0
    @Published var progress: Double? = nil
    @Published var statusLine = ""
    @Published var currentRoute: String? = nil
    @Published var batchRoutes: [String] = []
    @Published var doneRoutes: Set<String> = []
    @Published var failedRoutes: Set<String> = []

    private var proc: Process?
    private var pending: [Job] = []
    private var cancelled = false
    private var cfg: (out: String, chunks: String, del: Bool, audio: Bool, limit: Bool, script: String)?

    func startSync(output: String, chunks: String, autoDelete: Bool, syncAudio: Bool, limitPower: Bool, script: String) {
        begin(jobs: [Job(route: nil, args: ["sync"])], routes: [],
              output: output, chunks: chunks, autoDelete: autoDelete, syncAudio: syncAudio, limitPower: limitPower, script: script)
    }
    func startBatch(routes: [String], output: String, chunks: String, autoDelete: Bool,
                    syncAudio: Bool, limitPower: Bool, script: String) {
        begin(jobs: routes.map { Job(route: $0, args: ["restitch", $0]) }, routes: routes,
              output: output, chunks: chunks, autoDelete: autoDelete, syncAudio: syncAudio, limitPower: limitPower, script: script)
    }

    private func begin(jobs: [Job], routes: [String], output: String, chunks: String,
                       autoDelete: Bool, syncAudio: Bool, limitPower: Bool, script: String) {
        guard !isRunning, !jobs.isEmpty else { return }
        guard FileManager.default.fileExists(atPath: script) else {
            log = "ERROR: the comma-sync core wasn't found at:\n\(script)\n"; return
        }
        pending = jobs
        cfg = (output, chunks, autoDelete, syncAudio, limitPower, script)
        cancelled = false
        batchTotal = jobs.count
        batchDone = 0
        batchRoutes = routes
        doneRoutes = []
        failedRoutes = []
        progress = nil
        statusLine = ""
        log = ""
        isRunning = true
        startNext()
    }

    private func startNext() {
        guard !cancelled, !pending.isEmpty, let cfg = cfg else {
            isRunning = false
            currentRoute = nil
            log += cancelled ? "\n— stopped —\n" : "\n— all done —\n"
            return
        }
        let job = pending.removeFirst()
        currentRoute = job.route
        progress = nil
        statusLine = ""

        let p = Process()
        p.executableURL = URL(fileURLWithPath: cfg.script)   // the bundled Go core binary
        p.arguments = job.args + ["--json"]
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        env["ROOT"] = cfg.out
        if !cfg.chunks.isEmpty { env["CHUNKS_DIR"] = cfg.chunks }
        env["CLEAN_RAW"] = cfg.del ? "1" : "0"
        env["WITH_AUDIO"] = cfg.audio ? "1" : "0"
        if cfg.limit { env["BWLIMIT"] = "3m" }   // throttle to ~3 MB/s for weak power sources
        let ud = UserDefaults.standard            // combined multi-angle video options
        env["WITH_COMBINED"] = ud.bool(forKey: "withCombined") ? "1" : "0"
        env["PRIMARY_CAM"] = ud.string(forKey: "primaryCam") ?? "road"
        env["SECONDARY_CAM"] = ud.string(forKey: "secondaryCam") ?? "wide"
        env["TERTIARY_CAM"] = ud.string(forKey: "tertiaryCam") ?? "driver"
        p.environment = env

        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let d = h.availableData
            guard !d.isEmpty, let s = String(data: d, encoding: .utf8) else { return }
            DispatchQueue.main.async { self?.handle(s) }
        }
        p.terminationHandler = { [weak self] pr in
            pipe.fileHandleForReading.readabilityHandler = nil
            let ok = (pr.terminationStatus == 0)
            DispatchQueue.main.async {
                guard let self = self else { return }
                if let r = job.route, !self.cancelled {
                    if ok { self.doneRoutes.insert(r) } else { self.failedRoutes.insert(r) }
                }
                self.batchDone += 1
                self.currentRoute = nil
                self.startNext()
            }
        }
        do { try p.run(); proc = p }
        catch { log += "ERROR launching: \(error.localizedDescription)\n"; isRunning = false }
    }

    private func handle(_ chunk: String) {
        for raw in chunk.split(whereSeparator: { $0 == "\n" }) {
            let line = String(raw).trimmingCharacters(in: .whitespaces)
            if line.isEmpty { continue }
            // The core emits one JSON event per line on stdout; stderr (discovery
            // notes, etc.) is plain text and just goes to the log.
            guard let data = line.data(using: .utf8),
                  let ev = try? JSONDecoder().decode(CoreEvent.self, from: data) else {
                log += line + "\n"
                continue
            }
            switch ev.type {
            case "progress":
                if ev.phase == "stitch" {
                    progress = nil
                    statusLine = "Stitching video…"
                } else {
                    progress = (ev.percent ?? 0) / 100.0
                    statusLine = "Downloading" + (ev.route.map { " · \($0)" } ?? "")
                }
            case "drive", "log", "done":
                progress = nil
                if let m = ev.message, !m.isEmpty { log += m + "\n" }
            case "error":
                if let m = ev.message { log += "!! " + m + "\n" }
            default:
                break
            }
        }
    }

    func cancel() {
        cancelled = true
        pending.removeAll()
        log += "\n==> Stopping…\n"
        if let pid = proc?.processIdentifier, pid > 0 {
            killTree(pid, SIGTERM)
            DispatchQueue.global().asyncAfter(deadline: .now() + 1.5) { killTree(pid, SIGKILL) }
        }
    }
}

// One JSON event line from the Go core's --json stream.
struct CoreEvent: Decodable {
    let type: String
    let route: String?
    let phase: String?
    let percent: Double?
    let message: String?
}

struct Drive: Identifiable, Codable {
    let route: String
    let stamp: String
    let cameras: [String]   // core --json emits an array (road/wide/driver)
    let hasAudio: Bool?
    let sizeKB: Int
    let segments: Int
    let location: String
    var id: String { route }
    var onDevice: Bool { location == "device" }
    var sizeText: String {
        let mb = Double(sizeKB) / 1024.0
        return mb >= 1024 ? String(format: "%.1f GB", mb / 1024) : String(format: "%.0f MB", mb)
    }
    var subtitle: String {
        var parts = [cameras.joined(separator: ", ")]
        if let a = hasAudio { parts.append(a ? "audio" : "no audio") }
        parts.append(sizeText)
        parts.append("\(segments) min")
        return parts.joined(separator: " · ")
    }
}

struct ContentView: View {
    @AppStorage("outputDir") private var outputDir = ""
    @AppStorage("chunksDir") private var chunksDir = ""
    @AppStorage("autoDelete") private var autoDelete = false
    @AppStorage("syncAudio") private var syncAudio = true
    @AppStorage("limitPower") private var limitPower = false
    @AppStorage("autoUpdateCheck") private var autoUpdateCheck = true
    @AppStorage("withCombined") private var withCombined = false
    @AppStorage("primaryCam") private var primaryCam = "road"
    @AppStorage("secondaryCam") private var secondaryCam = "wide"
    @AppStorage("tertiaryCam") private var tertiaryCam = "driver"
    @StateObject private var runner = SyncRunner()
    @State private var showDrives = false
    @State private var drives: [Drive] = []
    @State private var loadingDrives = false
    @State private var updateVersion: String? = nil
    @State private var updateURL = ""

    // The bundled Go core binary (Resources/comma-sync). COMMA_SYNC_BIN overrides
    // it for development.
    private var scriptPath: String {
        if let p = ProcessInfo.processInfo.environment["COMMA_SYNC_BIN"], !p.isEmpty { return p }
        return Bundle.main.bundlePath + "/Contents/Resources/comma-sync"
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            if let v = updateVersion {
                HStack(spacing: 10) {
                    Image(systemName: "arrow.down.circle.fill").font(.title3).foregroundStyle(.tint)
                    VStack(alignment: .leading, spacing: 1) {
                        Text("Update available — \(v)").font(.callout).bold()
                        Text("You're on \(appVersion). Download the latest version.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Download") {
                        if let u = URL(string: updateURL) { NSWorkspace.shared.open(u) }
                    }.buttonStyle(.borderedProminent)
                    Button { updateVersion = nil } label: { Image(systemName: "xmark") }
                        .buttonStyle(.plain).foregroundStyle(.secondary)
                }
                .padding(10)
                .background(RoundedRectangle(cornerRadius: 8).fill(Color.accentColor.opacity(0.12)))
            }
            HStack(spacing: 12) {
                Image(systemName: "car.side.fill").font(.system(size: 30)).foregroundStyle(.tint)
                VStack(alignment: .leading, spacing: 2) {
                    Text("Comma Sync Go").font(.title).bold()
                    Text("Pull new drives off your comma and stitch them into videos")
                        .font(.subheadline).foregroundStyle(.secondary)
                }
            }

            HStack(spacing: 8) {
                Image(systemName: "flask.fill").foregroundStyle(.orange)
                Text("Beta — this build runs the new shared **Go core** engine (the same one that powers the Linux & Windows apps). Those haven't been hardware‑tested yet — please test and report any bugs.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color.orange.opacity(0.12)))

            GroupBox {
                VStack(spacing: 12) {
                    folderRow(title: "Stitched videos", systemImage: "film.stack", path: $outputDir)
                    Divider()
                    folderRow(title: "Raw HEVC chunks", systemImage: "shippingbox", path: $chunksDir)
                }
                .padding(6)
            }

            Toggle(isOn: $syncAudio) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Include microphone audio")
                    Text("Adds the recorded audio to the video when it's available")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle(isOn: $autoDelete) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Delete raw chunks after stitching")
                    Text("Reclaims space automatically once a drive is rendered")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle(isOn: $limitPower) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Limit speed for weak power sources")
                    Text("Caps the transfer to ~3 MB/s to lower the comma's power draw — use if it reboots mid-transfer")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle(isOn: $withCombined) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Also make a combined multi-angle video")
                    Text("One extra file with all cameras — 2 side by side, or 3 with the main angle on top and the other two below")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            if withCombined {
                HStack(spacing: 16) {
                    camPicker("Primary (top)", $primaryCam)
                    camPicker("Bottom-left", $secondaryCam)
                    camPicker("Bottom-right", $tertiaryCam)
                }
                .padding(.leading, 2)
            }
            Toggle(isOn: $autoUpdateCheck) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Automatically check for updates")
                    Text("Tells you when a newer version is on GitHub — only reads the public releases list, sends no data")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .onChange(of: autoUpdateCheck) { _, on in
                if on { checkForUpdate() } else { updateVersion = nil }
            }

            HStack(spacing: 10) {
                if runner.isRunning {
                    Button(role: .destructive) { runner.cancel() } label: {
                        Label("Stop", systemImage: "stop.circle.fill").frame(maxWidth: .infinity)
                    }
                    .controlSize(.large).buttonStyle(.borderedProminent).tint(.red)
                } else {
                    Button(action: syncNow) {
                        Label("Sync New", systemImage: "arrow.down.circle.fill").frame(maxWidth: .infinity)
                    }
                    .controlSize(.large).buttonStyle(.borderedProminent).disabled(outputDir.isEmpty)
                }
                Button(action: openDrives) {
                    Label("Index Drives", systemImage: "list.bullet.rectangle")
                }
                .controlSize(.large)
            }

            if runner.isRunning {
                progressBlock
            }

            logView
        }
        .padding(22)
        .frame(width: 580, height: 772)
        .onAppear {
            setDefaults()
            if drives.isEmpty { drives = loadCachedDrives() }
            checkForUpdate()
        }
        .task {
            // Re-check every 12 hours so the app left running still notices new
            // releases (checkForUpdate() no-ops while the toggle is off).
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(12 * 60 * 60))
                checkForUpdate()
            }
        }
        .sheet(isPresented: $showDrives) {
            DrivesSheet(drives: drives, isLoading: loadingDrives, runner: runner,
                        onBatch: { routes in
                            guard !routes.isEmpty, !runner.isRunning else { return }
                            runner.startBatch(routes: routes, output: outputDir, chunks: chunksDir,
                                              autoDelete: autoDelete, syncAudio: syncAudio,
                                              limitPower: limitPower, script: scriptPath)
                        },
                        onRefresh: { refreshDrives() },
                        onClose: { showDrives = false })
        }
    }

    private var progressBlock: some View {
        VStack(alignment: .leading, spacing: 6) {
            if runner.batchTotal > 1 {
                Text("Drive \(min(runner.batchDone + 1, runner.batchTotal)) of \(runner.batchTotal)")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if let p = runner.progress {
                ProgressView(value: p)
                HStack { Text("\(Int(p * 100))%"); Spacer(); Text(runner.statusLine) }
                    .font(.caption).foregroundStyle(.secondary)
            } else {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text(runner.statusLine.isEmpty ? "Working…" : runner.statusLine)
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
    }

    private var logView: some View {
        ScrollViewReader { sp in
            ScrollView {
                Text(runner.log.isEmpty ? "Output will appear here…" : runner.log)
                    .font(.system(.caption, design: .monospaced))
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .foregroundStyle(runner.log.isEmpty ? .secondary : .primary)
                    .textSelection(.enabled)
                Color.clear.frame(height: 1).id("END")
            }
            .padding(8)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .textBackgroundColor)))
            .overlay(RoundedRectangle(cornerRadius: 8).stroke(.quaternary))
            .frame(maxHeight: .infinity)
            .onChange(of: runner.log) { _, _ in withAnimation { sp.scrollTo("END", anchor: .bottom) } }
        }
    }

    @ViewBuilder
    private func camPicker(_ title: String, _ sel: Binding<String>) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.caption2).foregroundStyle(.secondary)
            Picker("", selection: sel) {
                Text("Road").tag("road")
                Text("Wide").tag("wide")
                Text("Driver").tag("driver")
            }
            .labelsHidden()
            .frame(width: 110)
        }
    }

    private func folderRow(title: String, systemImage: String, path: Binding<String>) -> some View {
        HStack(spacing: 10) {
            Image(systemName: systemImage).foregroundStyle(.secondary).frame(width: 20)
            VStack(alignment: .leading, spacing: 1) {
                Text(title).font(.callout).bold()
                Text(path.wrappedValue.isEmpty ? "Not set" : path.wrappedValue)
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
            }
            Spacer()
            Button("Choose…") { choose(path) }
        }
    }

    private func choose(_ binding: Binding<String>) {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.canCreateDirectories = true
        panel.prompt = "Select"
        if panel.runModal() == .OK, let url = panel.url { binding.wrappedValue = url.path }
    }

    private func setDefaults() {
        if outputDir.isEmpty {
            let appDir = (Bundle.main.bundlePath as NSString).deletingLastPathComponent
            outputDir = appDir + "/Comma Footage"
        }
        if chunksDir.isEmpty { chunksDir = outputDir + "/Raw HEVC Chunks" }
    }

    // ---- update check -------------------------------------------------------
    // This build's version, from Info.plist (build.sh sets it via APP_VERSION).
    private var appVersion: String {
        (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? "0"
    }

    // Ask the bundled core whether a newer Go-core beta is out — it checks the
    // gui-v* pre-releases (where this build lives), so testers are pointed at the
    // next beta, not the stable script-based app. Read-only; sends no user data.
    private func checkForUpdate() {
        guard autoUpdateCheck else { return }
        let core = scriptPath
        let version = appVersion
        DispatchQueue.global().async {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: core)
            p.arguments = ["update-check", "--current", version, "--prefix", "gui-v", "--prereleases", "--json"]
            var env = ProcessInfo.processInfo.environment
            env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
            p.environment = env
            let out = Pipe()
            p.standardOutput = out
            p.standardError = Pipe()
            do { try p.run() } catch { return }
            let data = out.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            struct U: Decodable { let updateAvailable: Bool; let tag: String?; let url: String? }
            guard let u = try? JSONDecoder().decode(U.self, from: data),
                  u.updateAvailable, let tag = u.tag, let url = u.url else { return }
            DispatchQueue.main.async {
                updateVersion = tag
                updateURL = url
            }
        }
    }

    private func syncNow() {
        runner.startSync(output: outputDir, chunks: chunksDir, autoDelete: autoDelete,
                         syncAudio: syncAudio, limitPower: limitPower, script: scriptPath)
    }

    // Open the indexing page. Re-index only when idle (avoid a second SSH session
    // mid-transfer); while running, just reopen to watch live progress.
    // Open the page WITHOUT re-scanning — show the last results (cached in memory
    // and persisted across launches). Only scan on the very first open, or when the
    // user taps Refresh. This keeps it instant to get back to and re-stitch from.
    private func openDrives() {
        // Load the saved list from disk BEFORE presenting the sheet, so the sheet
        // never renders an empty frame first. Always reflect the saved list if we
        // have one — even if the in-memory copy got reset.
        let cached = loadCachedDrives()
        if !cached.isEmpty { drives = cached }
        showDrives = true
        // Only auto-scan the very first time (nothing saved yet). After that, keep
        // showing the saved list until the user explicitly taps Refresh — even
        // across app restarts and when the comma isn't on the network.
        if drives.isEmpty && !loadingDrives
            && UserDefaults.standard.data(forKey: "cachedDrives") == nil { refreshDrives() }
    }

    private func refreshDrives() {
        guard !loadingDrives else { return }
        loadingDrives = true
        DispatchQueue.global(qos: .userInitiated).async {
            let result = loadDrives()
            DispatchQueue.main.async {
                loadingDrives = false
                // Keep the saved list if a scan comes back empty (comma offline,
                // moved chunks, etc.) instead of blanking what the user had.
                guard !result.isEmpty else { return }
                drives = result
                if let data = try? JSONEncoder().encode(result) {
                    UserDefaults.standard.set(data, forKey: "cachedDrives")
                }
            }
        }
    }

    private func loadCachedDrives() -> [Drive] {
        guard let data = UserDefaults.standard.data(forKey: "cachedDrives"),
              let d = try? JSONDecoder().decode([Drive].self, from: data) else { return [] }
        return d
    }

    private func loadDrives() -> [Drive] {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: scriptPath)   // the Go core
        p.arguments = ["list", "--json"]
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        env["ROOT"] = outputDir
        if !chunksDir.isEmpty { env["CHUNKS_DIR"] = chunksDir }
        p.environment = env
        let out = Pipe()
        p.standardOutput = out
        p.standardError = Pipe()
        do { try p.run() } catch { return [] }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let result = (try? JSONDecoder().decode([Drive].self, from: data)) ?? []
        return result.sorted { $0.stamp > $1.stamp }
    }
}

struct DrivesSheet: View {
    let drives: [Drive]
    let isLoading: Bool
    @ObservedObject var runner: SyncRunner
    let onBatch: ([String]) -> Void
    let onRefresh: () -> Void
    let onClose: () -> Void
    @State private var selection = Set<String>()

    private var allSelected: Bool { !drives.isEmpty && selection.count == drives.count }
    private var totalSizeText: String {
        let kb = drives.reduce(0) { $0 + $1.sizeKB }
        let gb = Double(kb) / 1024 / 1024
        return gb >= 1 ? String(format: "%.1f GB", gb) : String(format: "%.0f MB", Double(kb) / 1024)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Indexing Results").font(.title2).bold()
                    Text(headerSubtitle).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button { onRefresh() } label: { Image(systemName: "arrow.clockwise") }
                    .disabled(isLoading)
                    .help("Re-scan for the comma and refresh the list")
                Button("Done") { onClose() }.keyboardShortcut(.cancelAction)
            }

            if isLoading {
                VStack(spacing: 10) {
                    ProgressView()
                    Text("Indexing drives on your comma…").font(.caption).foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if drives.isEmpty {
                VStack(spacing: 6) {
                    Image(systemName: "tray").font(.system(size: 28)).foregroundStyle(.secondary)
                    Text("No drives found").foregroundStyle(.secondary)
                    Text("Nothing on this Mac, and the comma wasn't reachable.")
                        .font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.center)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                if !runner.isRunning {
                    HStack {
                        Button(action: toggleAll) {
                            Image(systemName: allSelected ? "checkmark.circle.fill"
                                  : (selection.isEmpty ? "circle" : "minus.circle.fill"))
                            Text("Select all")
                        }
                        .buttonStyle(.plain)
                        Spacer()
                        Text("\(selection.count) selected").font(.caption).foregroundStyle(.secondary)
                    }
                }

                ScrollView {
                    VStack(spacing: 8) { ForEach(drives) { row($0) } }.padding(.vertical, 2)
                }

                if runner.isRunning {
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text("Transferring \(runner.doneRoutes.count)/\(runner.batchTotal) — you can close this and come back.")
                            .font(.caption).foregroundStyle(.secondary)
                        Spacer()
                        Button(role: .destructive) { runner.cancel() } label: { Text("Stop") }
                    }
                } else {
                    HStack(spacing: 10) {
                        Spacer()
                        Button("Download Selected") { onBatch(Array(selection)) }
                            .disabled(selection.isEmpty)
                        Button("Download All") { onBatch(drives.map { $0.route }) }
                            .buttonStyle(.borderedProminent)
                    }
                }
            }
        }
        .padding(20)
        .frame(width: 580, height: 560)
    }

    private var headerSubtitle: String {
        if isLoading { return "Scanning this Mac and your comma…" }
        if drives.isEmpty { return "" }
        return "\(drives.count) drives · \(totalSizeText) total — on this Mac and still on the comma"
    }

    @ViewBuilder
    private func row(_ d: Drive) -> some View {
        HStack(spacing: 12) {
            if !runner.isRunning {
                Button { toggle(d.route) } label: {
                    Image(systemName: selection.contains(d.route) ? "checkmark.circle.fill" : "circle")
                        .foregroundStyle(selection.contains(d.route) ? Color.accentColor : .secondary)
                }
                .buttonStyle(.plain)
            }

            Image(systemName: icon(d))
                .foregroundStyle(d.onDevice ? .secondary : (d.hasAudio == true ? Color.accentColor : .secondary))
                .frame(width: 22)
            VStack(alignment: .leading, spacing: 2) {
                Text(d.stamp).font(.callout).bold()
                Text(d.subtitle).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            statusAccessory(d)
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .controlBackgroundColor)))
    }

    @ViewBuilder
    private func statusAccessory(_ d: Drive) -> some View {
        if runner.currentRoute == d.route {
            if let p = runner.progress {
                HStack(spacing: 8) {
                    ProgressView(value: p).frame(width: 90)
                    Text("\(Int(p * 100))%").font(.caption).foregroundStyle(.secondary).monospacedDigit()
                }
            } else {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text(runner.statusLine.isEmpty ? "Starting…" : runner.statusLine)
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        } else if runner.doneRoutes.contains(d.route) {
            Label("Synced", systemImage: "checkmark.circle.fill")
                .font(.caption).foregroundStyle(.green).labelStyle(.titleAndIcon)
        } else if runner.failedRoutes.contains(d.route) {
            Label("Failed", systemImage: "exclamationmark.triangle.fill")
                .font(.caption).foregroundStyle(.orange)
        } else if runner.isRunning && runner.batchRoutes.contains(d.route) {
            Text("Queued").font(.caption).foregroundStyle(.secondary)
        } else {
            HStack(spacing: 10) {
                Text(d.onDevice ? "on comma" : "on Mac")
                    .font(.caption2).padding(.horizontal, 7).padding(.vertical, 3)
                    .background(Capsule().fill(Color(nsColor: .quaternaryLabelColor)))
                if !runner.isRunning {
                    Button(d.onDevice ? "Download" : "Re-stitch") { onBatch([d.route]) }
                }
            }
        }
    }

    private func icon(_ d: Drive) -> String {
        if d.onDevice { return "arrow.down.circle" }
        return d.hasAudio == true ? "speaker.wave.2.fill" : "film"
    }
    private func toggle(_ route: String) {
        if selection.contains(route) { selection.remove(route) } else { selection.insert(route) }
    }
    private func toggleAll() {
        if allSelected { selection.removeAll() } else { selection = Set(drives.map { $0.route }) }
    }
}

@main
struct CommaSyncApp: App {
    var body: some Scene {
        WindowGroup("Comma Sync Go") { ContentView() }
            .windowResizability(.contentSize)
    }
}
