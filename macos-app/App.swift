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
    @Published var rateMBps: Double? = nil
    @Published var batchLabel = ""
    @Published var currentRoute: String? = nil
    @Published var batchRoutes: [String] = []
    @Published var doneRoutes: Set<String> = []
    @Published var failedRoutes: Set<String> = []
    // Which drive of how many is being worked on right now. Sync New picks its own list
    // of drives, so these come from the core's plan/drive events — the app can't count
    // them itself, and before this the run gave no sense of how much was left.
    @Published var driveIndex = 0
    // Which pass is running. With "download everything first" the whole first pass
    // finishes nothing, so a done-tally would sit on 0 and read as stuck; what's moving
    // is the transfer count.
    @Published var drivePhase = ""
    @Published var transferredRoutes: Set<String> = []

    // "Drive 3/7" once the run has a shape, otherwise nothing to say yet.
    var driveCounter: String {
        guard batchTotal > 0, driveIndex > 0 else { return "" }
        return "Drive \(driveIndex)/\(batchTotal)"
    }
    // What's actually completing right now — transfers during the first pass, finished
    // drives once stitching starts.
    var tallyText: String {
        drivePhase == "download"
            ? "\(transferredRoutes.count) transferred"
            : "\(doneRoutes.count) done"
    }

    private var proc: Process?
    private var routeCount = 0
    private var twoPhase = false
    private var pending: [Job] = []
    private var cancelled = false
    private var cfg: (out: String, chunks: String, del: Bool, audio: Bool, limit: Bool, script: String)?

    func startSync(output: String, chunks: String, autoDelete: Bool, syncAudio: Bool, limitPower: Bool, script: String) {
        begin(jobs: [Job(route: nil, args: ["sync"])], routes: [],
              output: output, chunks: chunks, autoDelete: autoDelete, syncAudio: syncAudio, limitPower: limitPower, script: script)
    }
    func startBatch(routes: [String], output: String, chunks: String, autoDelete: Bool,
                    syncAudio: Bool, limitPower: Bool, script: String) {
        // One core process handles the whole batch, so IT controls the ordering:
        // with "download all drives first" on it finishes every transfer before any
        // stitching. Per-drive state comes back as events, not one process per drive.
        begin(jobs: [Job(route: nil, args: ["batch"] + routes)], routes: routes,
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
        // One `batch` job now carries ALL the drives, so jobs.count is 1 — count the
        // drives instead, or the progress line reads nonsense like "5/1".
        batchTotal = max(routes.count, jobs.count)
        batchDone = 0
        driveIndex = 0
        drivePhase = ""
        transferredRoutes = []
        batchRoutes = routes
        routeCount = routes.count
        twoPhase = routes.count > 1 && jobs.count > routes.count
        batchLabel = ""
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
        batchLabel = routeCount > 1 ? "\(routeCount) drives" : ""

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
        let allFirst = (ud.object(forKey: "syncAllFirst") as? Bool) ?? true
        env["SYNC_ORDER"] = allFirst ? "all-first" : "per-drive"
        env["WITH_360"] = ud.bool(forKey: "with360") ? "1" : "0"
        env["WITH_VERTICAL"] = ud.bool(forKey: "withVertical") ? "1" : "0"
        env["VERTICAL_DRIVER_POS"] = ud.string(forKey: "verticalDriverPos") ?? "bottom"
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
                // A download-phase job finishing doesn't mean the drive is done —
                // only the stitch pass marks it Synced.
                if let r = job.route, !self.cancelled, job.args.first != "download" {
                    if ok { self.doneRoutes.insert(r) } else { self.failedRoutes.insert(r) }
                } else if let r = job.route, !self.cancelled, !ok {
                    self.failedRoutes.insert(r)
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
                    rateMBps = nil
                    statusLine = "Stitching video…"
                } else if ev.phase == "analyze" {
                    // Measuring camera alignment/timing before the encode — its own step,
                    // so the message reads on its own rather than as "Rendering …".
                    rateMBps = nil
                    progress = (ev.percent ?? 0) / 100.0
                    statusLine = (ev.message ?? "Analyzing") + "…"
                } else if ev.phase == "render" {
                    // A long re-encode (combined / 360 / vertical). These can run for many
                    // minutes on a multi-hour drive, so show real percent — with no bar at
                    // all it was indistinguishable from the app having hung.
                    rateMBps = nil
                    progress = (ev.percent ?? 0) / 100.0
                    statusLine = "Rendering " + (ev.message ?? "video") + "…"
                } else {
                    progress = (ev.percent ?? 0) / 100.0
                    if let r = ev.rateMBps, r > 0 { rateMBps = r }
                    statusLine = "Downloading" + (ev.route.map { " · \($0)" } ?? "")
                }
                if let r = ev.route, !r.isEmpty { currentRoute = r }
            case "routedone":
                if let r = ev.route, !r.isEmpty {
                    // A finished transfer isn't a finished drive — it only counts as done
                    // once it's stitched, which is why this is tallied separately.
                    if ev.phase == "download" {
                        transferredRoutes.insert(r)
                    } else {
                        doneRoutes.insert(r)
                        if currentRoute == r { currentRoute = nil }
                    }
                }
            case "plan":
                // The core has worked out how many drives this run covers. Sync New starts
                // with no list at all, so this is the only place the total comes from.
                if let t = ev.total, t > 0 { batchTotal = t }
                driveIndex = 0
                if let m = ev.message, !m.isEmpty { log += m + "\n" }
            case "drive":
                progress = nil
                if let i = ev.index, i > 0 { driveIndex = i }
                if let t = ev.total, t > 0 { batchTotal = t }
                if let r = ev.route, !r.isEmpty { currentRoute = r }
                drivePhase = ev.phase ?? ""
                batchLabel = driveCounter
                if let m = ev.message, !m.isEmpty { log += "==> " + m + "\n" }
            case "log", "done":
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
    let rateMBps: Double?
    let message: String?
    let index: Int?      // this drive's position in the run
    let total: Int?      // how many drives the run covers
}

struct Drive: Identifiable, Codable {
    let route: String
    let stamp: String
    let cameras: [String]   // core --json emits an array (road/wide/driver)
    let hasAudio: Bool?
    let sizeKB: Int
    let segments: Int
    let location: String
    // Already stitched into the output folder. Independent of location: a drive can be
    // on the comma AND already synced, and the index has to be able to say both.
    let synced: Bool?
    var id: String { route }
    var alreadySynced: Bool { synced == true }
    var onDevice: Bool { location == "device" }
    // Only the stitched per-camera videos remain (raw chunks gone, not on the comma).
    // These can still gain new derived outputs (combined/360/vertical) from the videos.
    var stitchedOnly: Bool { location == "stitched" }
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
    @AppStorage("syncAllFirst") private var syncAllFirst = true
    @AppStorage("autoUpdateCheck") private var autoUpdateCheck = true
    @AppStorage("withCombined") private var withCombined = false
    @AppStorage("primaryCam") private var primaryCam = "road"
    @AppStorage("secondaryCam") private var secondaryCam = "wide"
    @AppStorage("tertiaryCam") private var tertiaryCam = "driver"
    @AppStorage("with360") private var with360 = false
    @AppStorage("withVertical") private var withVertical = false
    @AppStorage("verticalDriverPos") private var verticalDriverPos = "bottom"
    @StateObject private var runner = SyncRunner()
    @State private var showDrives = false
    @State private var drives: [Drive] = []
    @State private var loadingDrives = false
    @State private var scanOffline = false
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
                    Text("Comma Sync").font(.title).bold()
                    Text("Pull new drives off your comma and stitch them into videos")
                        .font(.subheadline).foregroundStyle(.secondary)
                }
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
            Toggle(isOn: $syncAllFirst) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Download all drives first, then stitch")
                    Text("Finishes every transfer while the comma is reachable, then renders afterwards. Off = stitch each drive right after it downloads.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle(isOn: $withCombined) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Make a combined multi-angle video")
                    Text("One extra file — 2 angles side by side, or 3 with the primary on top and the other two below. Set Tertiary to None to combine just two even when a third camera exists.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            if withCombined {
                HStack(spacing: 16) {
                    camPicker("Primary", $primaryCam)
                    camPicker("Secondary", $secondaryCam)
                    camPicker("Tertiary", $tertiaryCam, allowNone: true)
                }
                .padding(.leading, 2)
            }
            Toggle(isOn: $with360) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Make a 360° VR video")
                    Text("An equirectangular file for a VR headset — wide cam in front, driver cam behind, road cam sharpened in the center. Needs all three cameras.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle(isOn: $withVertical) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("Make a vertical video")
                    Text("A portrait file made for phone screens — the wide cam (with the road cam sharpened over its center) stacked with the driver cam.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            if withVertical {
                HStack(spacing: 10) {
                    Text("Driver cam").font(.caption2).foregroundStyle(.secondary)
                    Picker("", selection: $verticalDriverPos) {
                        Text("Bottom").tag("bottom")
                        Text("Top").tag("top")
                    }
                    .pickerStyle(.segmented).labelsHidden().frame(width: 160)
                    Spacer()
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
                    // The index page stays reachable mid-sync — it shows live
                    // per-drive progress and can queue nothing while running.
                    Button(action: openDrives) {
                        Label("Index Drives", systemImage: "list.bullet.rectangle")
                    }
                    .controlSize(.large)
                } else {
                    // Index Drives is the one entry point: browse everything and pick
                    // what to download. Sync New lives on that page (the rarer path).
                    Button(action: openDrives) {
                        Label("Index Drives", systemImage: "list.bullet.rectangle").frame(maxWidth: .infinity)
                    }
                    .controlSize(.large).buttonStyle(.borderedProminent).disabled(outputDir.isEmpty)
                }
            }

            if runner.isRunning {
                progressBlock
            }

            logView
        }
        .padding(22)
        // Width is fixed; height hugs the content so the window grows/shrinks as the
        // conditional option rows (combined roles, vertical driver position) appear —
        // the log keeps its minimum height instead of getting crushed.
        .frame(width: 580)
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
            DrivesSheet(drives: drives, isLoading: loadingDrives, offline: scanOffline, runner: runner,
                        onBatch: { routes in
                            guard !routes.isEmpty, !runner.isRunning else { return }
                            runner.startBatch(routes: routes, output: outputDir, chunks: chunksDir,
                                              autoDelete: autoDelete, syncAudio: syncAudio,
                                              limitPower: limitPower, script: scriptPath)
                        },
                        onSyncNew: { syncNow() },
                        onRefresh: { refreshDrives() },
                        onClose: { showDrives = false })
        }
    }

    private var progressBlock: some View {
        VStack(alignment: .leading, spacing: 6) {
            if !runner.batchLabel.isEmpty {
                Text(runner.batchLabel).font(.caption).foregroundStyle(.secondary)
            }
            if let p = runner.progress {
                ProgressView(value: p)
                HStack {
                    Text("\(Int(p * 100))%")
                    if let r = runner.rateMBps {
                        Text(String(format: "· %.1f MB/s", r)).monospacedDigit()
                    }
                    Spacer(); Text(runner.statusLine)
                }
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
            .frame(minHeight: 240, maxHeight: .infinity)
            .onChange(of: runner.log) { _, _ in withAnimation { sp.scrollTo("END", anchor: .bottom) } }
        }
    }

    @ViewBuilder
    private func camPicker(_ title: String, _ sel: Binding<String>, allowNone: Bool = false) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.caption2).foregroundStyle(.secondary)
            Picker("", selection: sel) {
                Text("Road").tag("road")
                Text("Wide").tag("wide")
                Text("Driver").tag("driver")
                // Only the tertiary slot may be empty: with primary + secondary alone the
                // combined video is the two of them side by side. A "none" secondary would
                // just duplicate the primary's individual video, so it isn't offered.
                if allowNone { Text("None").tag("none") }
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

    // Ask the bundled core whether a newer release is out — it checks the stable
    // GitHub releases (the latest official build). Read-only; sends no user data.
    private func checkForUpdate() {
        guard autoUpdateCheck else { return }
        let core = scriptPath
        let version = appVersion
        DispatchQueue.global().async {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: core)
            p.arguments = ["update-check", "--current", version, "--json"]
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
                guard !result.isEmpty else { scanOffline = true; return }
                var merged = result
                // If the comma wasn't reachable, this scan is local-only. Keep the
                // cached device-side drives instead of silently dropping them —
                // otherwise a reload while offline "loses" drives and can change
                // what the list shows compared to the last good scan.
                let sawDevice = result.contains { $0.location == "device" }
                scanOffline = !sawDevice
                if !sawDevice {
                    let haveRoutes = Set(result.map { $0.route })
                    // Also match on the recording time, not just the route. A drive that
                    // has been stitched is listed under its timestamp folder, which is a
                    // different string from the comma's route id — so matching by route
                    // alone let the same drive appear twice: once as a synced drive and
                    // again as a stale "on comma" row with no audio recorded against it.
                    let haveStamps = Set(result.map { $0.stamp })
                    let cached = loadCachedDrives()
                    merged += cached.filter {
                        $0.location == "device"
                            && !haveRoutes.contains($0.route)
                            && !haveStamps.contains($0.stamp)
                    }
                    merged.sort { $0.stamp > $1.stamp }
                }
                drives = merged
                if let data = try? JSONEncoder().encode(merged) {
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
    let offline: Bool
    @ObservedObject var runner: SyncRunner
    let onBatch: ([String]) -> Void
    let onSyncNew: () -> Void
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
                        // Phase-neutral: a batch may be transferring OR re-encoding here.
                        // The drive counter leads, because "which of how many" is the thing
                        // you want when deciding whether to wait around.
                        Text(runner.driveCounter.isEmpty
                             ? "\(runner.tallyText) — you can close this and come back."
                             : "\(runner.driveCounter) · \(runner.tallyText) — you can close this and come back.")
                            .font(.caption).foregroundStyle(.secondary)
                        Spacer()
                        Button(role: .destructive) { runner.cancel() } label: { Text("Stop") }
                    }
                } else {
                    HStack(spacing: 10) {
                        Button {
                            onSyncNew()
                        } label: {
                            Label("Sync New", systemImage: "arrow.down.circle.fill")
                        }
                        .help("Download and stitch every new drive on the comma that isn't synced yet")
                        Spacer()
                        Button("Restitch Selected") { onBatch(Array(selection)) }
                            .disabled(selection.isEmpty)
                            .help("Re-render the selected drives from footage already downloaded — e.g. to apply a new multi-angle layout. Skips any that are already rendered with the current settings.")
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
        if offline {
            return "\(drives.count) drives · \(totalSizeText) total — comma offline, showing the last known device list"
        }
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
                .foregroundStyle(d.hasAudio == true ? Color.accentColor : .secondary)
                .frame(width: 22)
                .help(iconHelp(d))
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
                    Text("\(Int(p * 100))%" + (runner.rateMBps.map { String(format: " · %.1f MB/s", $0) } ?? ""))
                        .font(.caption).foregroundStyle(.secondary).monospacedDigit()
                }
            } else {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text(runner.statusLine.isEmpty ? "Starting…" : runner.statusLine)
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        } else if runner.isRunning {
            // A batch is running and this isn't the drive being processed right now.
            if runner.doneRoutes.contains(d.route) {
                Label("Synced", systemImage: "checkmark.circle.fill")
                    .font(.caption).foregroundStyle(.green).labelStyle(.titleAndIcon)
            } else if runner.failedRoutes.contains(d.route) {
                Label("Failed", systemImage: "exclamationmark.triangle.fill")
                    .font(.caption).foregroundStyle(.orange)
            } else if runner.transferredRoutes.contains(d.route) {
                // Downloaded but not stitched yet — distinct from both queued and done.
                Label("Transferred", systemImage: "internaldrive")
                    .font(.caption).foregroundStyle(.secondary)
            } else if runner.batchRoutes.contains(d.route) {
                Text("Queued").font(.caption).foregroundStyle(.secondary)
            }
        } else {
            // Idle: show a glyph for what happened this session, and ALWAYS offer the
            // action button — a synced (green) drive can still be re-stitched, e.g. to
            // apply a new multi-angle layout. (Before, the "Synced" label replaced the
            // button, so synced drives couldn't be re-stitched even after reloading.)
            HStack(spacing: 10) {
                if runner.doneRoutes.contains(d.route) {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                        .help("Synced this session")
                } else if runner.failedRoutes.contains(d.route) {
                    Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                        .help("Last attempt failed")
                } else {
                    // Where the footage is and whether it's been stitched are two separate
                    // facts, so they get two tags. A drive still on the comma that's already
                    // in the output folder used to read as untouched work.
                    HStack(spacing: 5) {
                        badge(d.stitchedOnly ? "videos only" : (d.onDevice ? "on comma" : "on Mac"))
                            .help(d.stitchedOnly ? "Raw chunks are gone — rebuild new outputs (combined/360/vertical) from the stitched videos" : "")
                        // "videos only" already means it was stitched — don't say it twice.
                        if d.alreadySynced && !d.stitchedOnly {
                            badge("synced", tint: .green)
                                .help("Already stitched — the videos are in your output folder")
                        }
                    }
                }
                Button(buttonTitle(d)) {
                    onBatch([d.route])
                }
            }
        }
    }

    // Where a drive lives is already spelled out by the badge and the action button, so
    // this glyph says one thing only: whether the mic was recording. Crucially, "not
    // known yet" must not look like "no audio" — that was the bug. A drive still on the
    // comma was never probed for audio, and the fallback glyph read as silent.
    private func icon(_ d: Drive) -> String {
        if let a = d.hasAudio { return a ? "speaker.wave.2.fill" : "speaker.slash" }
        if d.stitchedOnly { return "wand.and.stars" }
        return d.onDevice ? "arrow.down.circle" : "film"
    }
    @ViewBuilder
    private func badge(_ text: String, tint: Color? = nil) -> some View {
        Text(text)
            .font(.caption2)
            .foregroundStyle(tint ?? .primary)
            .padding(.horizontal, 7).padding(.vertical, 3)
            .background(Capsule().fill(tint?.opacity(0.15) ?? Color(nsColor: .quaternaryLabelColor)))
    }
    private func iconHelp(_ d: Drive) -> String {
        guard let a = d.hasAudio else { return "Audio not known — the drive hasn't been read yet" }
        return a ? "Recorded with audio" : "No audio in this drive"
    }
    // Video-only drives have no chunks to fetch/re-stitch, so the action is a rebuild of
    // the derived outputs; everything else keeps Download / Re-stitch.
    private func buttonTitle(_ d: Drive) -> String {
        if d.stitchedOnly { return "Rebuild" }
        return (runner.doneRoutes.contains(d.route) || !d.onDevice) ? "Re-stitch" : "Download"
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
        WindowGroup("Comma Sync") { ContentView() }
            .windowResizability(.contentSize)
    }
}
