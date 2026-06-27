// Comma Sync GUI (Tauri) — a thin cross-platform front-end that drives the
// `comma-sync` core binary and streams its --json events to the web UI.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::io::{BufRead, BufReader};
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;

use serde::Deserialize;
use tauri::{AppHandle, Emitter, State};
use tauri_plugin_dialog::DialogExt;

#[derive(Default)]
struct AppState {
    child: Mutex<Option<Child>>,
}

#[derive(Deserialize)]
struct Opts {
    output: String,
    chunks: String,
    audio: bool,
    clean: bool,
    #[serde(default)]
    use_usb: bool,
}

/// Locate the core binary: $COMMA_SYNC_BIN, then next to this app, then PATH.
fn core_bin() -> String {
    if let Ok(p) = std::env::var("COMMA_SYNC_BIN") {
        return p;
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            for name in ["comma-sync", "comma-sync.exe"] {
                let cand = dir.join(name);
                if cand.exists() {
                    return cand.to_string_lossy().into_owned();
                }
            }
        }
    }
    "comma-sync".into()
}

fn core_command(opts: &Opts, args: &[&str]) -> Command {
    let mut cmd = Command::new(core_bin());
    cmd.args(args);
    cmd.env("ROOT", &opts.output);
    if !opts.chunks.is_empty() {
        cmd.env("CHUNKS_DIR", &opts.chunks);
    }
    cmd.env("WITH_AUDIO", if opts.audio { "1" } else { "0" });
    cmd.env("CLEAN_RAW", if opts.clean { "1" } else { "0" });
    if opts.use_usb {
        cmd.env("USE_USB", "1");
    }
    cmd
}

#[tauri::command]
fn pick_folder(app: AppHandle) -> Option<String> {
    app.dialog()
        .file()
        .blocking_pick_folder()
        .map(|p| p.to_string())
}

#[tauri::command]
fn list_drives(opts: Opts) -> Result<serde_json::Value, String> {
    let out = core_command(&opts, &["list", "--json"])
        .output()
        .map_err(|e| format!("running core: {e}"))?;
    if !out.status.success() {
        return Err(String::from_utf8_lossy(&out.stderr).into_owned());
    }
    serde_json::from_slice(&out.stdout).map_err(|e| format!("parsing list: {e}"))
}

/// Ask the core whether a newer GUI release exists (reads GitHub's public release
/// list only; sends nothing). Returns {updateAvailable, latest, tag, url}.
#[tauri::command]
fn check_update(app: AppHandle) -> Result<serde_json::Value, String> {
    let version = app.package_info().version.to_string();
    let out = Command::new(core_bin())
        .args([
            "update-check", "--current", &version,
            "--prefix", "gui-v", "--prereleases", "--json",
        ])
        .output()
        .map_err(|e| format!("running core: {e}"))?;
    if !out.status.success() {
        return Err(String::from_utf8_lossy(&out.stderr).into_owned());
    }
    serde_json::from_slice(&out.stdout).map_err(|e| format!("parsing update: {e}"))
}

/// Open a URL in the default browser (the update banner's Download button).
#[tauri::command]
fn open_url(url: String) {
    #[cfg(target_os = "windows")]
    let _ = Command::new("cmd").args(["/C", "start", "", &url]).spawn();
    #[cfg(target_os = "macos")]
    let _ = Command::new("open").arg(&url).spawn();
    #[cfg(target_os = "linux")]
    let _ = Command::new("xdg-open").arg(&url).spawn();
}

/// Run `sync` or `restitch …` and stream the core's --json events to the UI as
/// `core-event` (stdout lines), `core-stderr`, and `core-done`.
#[tauri::command]
fn start_job(
    app: AppHandle,
    state: State<AppState>,
    opts: Opts,
    args: Vec<String>,
) -> Result<(), String> {
    let mut argv = args;
    argv.push("--json".into());
    let refs: Vec<&str> = argv.iter().map(String::as_str).collect();

    let mut cmd = core_command(&opts, &refs);
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = cmd.spawn().map_err(|e| format!("spawn core: {e}"))?;

    let stdout = child.stdout.take().ok_or("no stdout")?;
    let stderr = child.stderr.take().ok_or("no stderr")?;
    *state.child.lock().unwrap() = Some(child);

    let a1 = app.clone();
    std::thread::spawn(move || {
        for line in BufReader::new(stdout).lines().map_while(Result::ok) {
            let _ = a1.emit("core-event", line);
        }
        let _ = a1.emit("core-done", ());
    });

    let a2 = app.clone();
    std::thread::spawn(move || {
        for line in BufReader::new(stderr).lines().map_while(Result::ok) {
            let _ = a2.emit("core-stderr", line);
        }
    });

    Ok(())
}

#[tauri::command]
fn cancel_job(state: State<AppState>) {
    if let Some(child) = state.child.lock().unwrap().as_mut() {
        let _ = child.kill();
    }
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .manage(AppState::default())
        .invoke_handler(tauri::generate_handler![
            pick_folder,
            list_drives,
            check_update,
            open_url,
            start_job,
            cancel_job
        ])
        .run(tauri::generate_context!())
        .expect("error while running Comma Sync");
}
