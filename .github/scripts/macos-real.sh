#!/bin/bash
# Real-desktop checks on a GitHub macOS runner (a real macOS VM with a
# logged-in GUI user). Run by .github/workflows/macos-real.yml; needs no
# secrets and makes no AI calls (fake agents only).
#
#   OUT=dir RW=path/to/rw .github/scripts/macos-real.sh
#
# Every check prints PASS, FAIL or INFO into $OUT/summary.txt. INFO is for
# what the runner does not let us see; FAIL is a Relayweft problem. The
# script exits 1 if anything failed.
set -u
OUT=${OUT:?}
RW=${RW:?}
ROOT=$(pwd)
T=${RUNNER_TEMP:-/tmp}/rw-real
rm -rf "$T"
mkdir -p "$OUT" "$T"
SUM="$OUT/summary.txt"
: >"$SUM"
fails=0
pass() { echo "PASS: $*" | tee -a "$SUM"; }
fail() { echo "FAIL: $*" | tee -a "$SUM"; fails=$((fails + 1)); }
info() { echo "INFO: $*" | tee -a "$SUM"; }
section() { echo | tee -a "$SUM"; echo "== $* ==" | tee -a "$SUM"; }
shot() {
	if screencapture -x "$OUT/$1.png" 2>>"$OUT/screencapture-errors.txt"; then
		info "screenshot $1.png"
	else
		info "screenshot $1 failed"
	fi
}
# tmo SECONDS cmd...: macOS has no timeout(1). An Apple Event waiting for a
# permission prompt nobody answers would otherwise hang for minutes.
tmo() {
	perl -e '$t = shift; $pid = fork; if (!$pid) { exec @ARGV or exit 127 }
		$SIG{ALRM} = sub { kill "KILL", $pid; exit 124 }; alarm $t; waitpid $pid, 0; exit($? >> 8)' "$@"
}
# "${NEWPG[@]}" cmd...: run cmd as the leader of its own process group, like a job
# a terminal starts (Ctrl+C there signals the whole group).
# (an array, not a function: a backgrounded function runs in a subshell, so
# $! would be the subshell, not rw).
NEWPG=(perl -e 'setpgrp(0, 0); exec @ARGV or exit 127')
# wait_file FILE PATTERN SECONDS
wait_file() {
	for _ in $(seq "$3"); do
		grep -q "$2" "$1" 2>/dev/null && return 0
		sleep 1
	done
	return 1
}
# wait_gone PID SECONDS: 0 when the process ended in time.
wait_gone() {
	for _ in $(seq "$2"); do
		kill -0 "$1" 2>/dev/null || return 0
		sleep 1
	done
	return 1
}

section "machine"
info "bash $BASH_VERSION"
info "$(sw_vers | tr '\n' ' ')"
info "arch $(uname -m), user $(id -un), console user $(stat -f %Su /dev/console)"
info "rw $("$RW" version)"
ls /Applications >"$OUT/applications.txt"
info "browsers in /Applications: $(ls /Applications | grep -Ei 'chrome|edge|firefox|brave|chromium|safari' | tr '\n' ',')"

# --- notifications ----------------------------------------------------------
section "desktop notifications (internal/notify via osascript)"
# Banners are drawn by NotificationCenter.app; the runner may not run it.
open -g /System/Library/CoreServices/NotificationCenter.app >/dev/null 2>&1
sleep 2
info "notification processes: $(pgrep -l -f 'NotificationCenter|usernoted' | tr '\n' ',')"
go test -c -o "$T/notify.test" ./internal/notify || fail "build notify test"
since=$(date '+%Y-%m-%d %H:%M:%S')
RW_REAL_DESKTOP=1 "$T/notify.test" -test.run 'TestRealDesktopToast$' -test.v >"$OUT/notify-test.txt" 2>&1
rc=$?
shot notify-1
sleep 1
shot notify-2
marker=$(grep -o 'marker sytest[0-9]*' "$OUT/notify-test.txt" | head -1 | cut -d' ' -f2)
sent=$(grep -c ': sent in ' "$OUT/notify-test.txt")
if [ $rc -eq 0 ] && [ "$sent" -eq 7 ]; then
	pass "notify.Send ran osascript without error for all 7 test notifications (marker $marker)"
else
	fail "notify.Send: test exit $rc, $sent of 7 sent (see notify-test.txt)"
fi
# Delivery 1: usernoted (the notification server) logs each one it accepts
# (macOS 15 and later; older versions log less).
sleep 2
log show --start "$since" --style compact --predicate 'process == "usernoted" OR process == "NotificationCenter"' >"$OUT/notify-unified-log.txt" 2>&1
delivering=$(grep -c 'Delivering <NotificationRecord app:"com.apple.ScriptEditor2"' "$OUT/notify-unified-log.txt")
banner=$(grep -c 'as banner' "$OUT/notify-unified-log.txt")
if [ "$delivering" -ge 7 ]; then
	pass "usernoted (the notification server) logged 'Delivering' for $delivering notifications from osascript (app com.apple.ScriptEditor2); 'as banner' $banner times"
elif [ "$delivering" -eq 0 ]; then
	info "usernoted logged nothing about them on this macOS (see notify-unified-log.txt)"
else
	fail "usernoted delivered only $delivering of 7 notifications (see notify-unified-log.txt)"
fi
# Delivery 2: read them back from the Notification Center database (macOS
# 15 and later in a group container, before that in DARWIN_USER_DIR). It is
# written lazily, so poll for up to 30s.
dbdir="$HOME/Library/Group Containers/group.com.apple.usernoted/db2"
[ -d "$dbdir" ] || dbdir="$(getconf DARWIN_USER_DIR)com.apple.notificationcenter/db2"
ls -la "$dbdir" >"$OUT/ncdb-ls.txt" 2>&1
hits=""
for _ in $(seq 10); do
	rm -rf "$T/ncdb"
	mkdir -p "$T/ncdb"
	cp "$dbdir"/db* "$T/ncdb/" 2>>"$OUT/ncdb-ls.txt" || break
	python3 - "$T/ncdb/db" "$marker" >"$OUT/ncdb-records.txt" 2>&1 <<'EOF'
import plistlib, sqlite3, sys
db, marker = sys.argv[1], sys.argv[2]
con = sqlite3.connect(db)
apps = dict(con.execute("select app_id, identifier from app"))
hits = 0
for rec_id, app_id, data, delivered in con.execute("select rec_id, app_id, data, delivered_date from record order by rec_id"):
    try:
        req = plistlib.loads(data).get("req", {})
    except Exception as e:
        req = {"error": str(e)}
    title, body = req.get("titl"), req.get("body")
    mine = marker in repr(req) or marker.encode() in data
    hits += mine
    print(rec_id, apps.get(app_id), delivered, "MINE" if mine else "", repr(title)[:120], "|", repr(body)[:160])
print("HITS", hits)
EOF
	hits=$(awk '/^HITS/ {print $2}' "$OUT/ncdb-records.txt")
	[ -n "$hits" ] && [ "$hits" -ge 7 ] && break
	sleep 3
done
if [ -z "$hits" ]; then
	info "Notification Center database not readable here (see ncdb-ls.txt, ncdb-records.txt)"
elif [ "$hits" -ge 1 ]; then
	app=$(grep ' MINE ' "$OUT/ncdb-records.txt" | awk '{print $2}' | sort -u | tr '\n' ' ')
	pass "Notification Center database holds $hits of 7 test notifications (app: $app)"
	grep ' MINE ' "$OUT/ncdb-records.txt" | cut -c1-200 >>"$SUM"
else
	info "the Notification Center database holds none of them after 30s (it is written lazily)"
fi
defaults read com.apple.ncprefs >"$OUT/ncprefs.txt" 2>&1

# --- keep-awake -------------------------------------------------------------
section "keep-awake during a scheduled wait (caffeinate)"
R="$T/repo"
mkdir -p "$R"
git -C "$R" init -q
echo hello >"$R/a.txt"
git -C "$R" add .
git -C "$R" -c user.name=ci -c user.email=ci@example.com commit -qm init
FB="$T/fakebin"
mkdir -p "$FB"
for a in claude codex; do
	printf '#!/bin/sh\necho "fake %s must not run in this check" >&2\nexit 1\n' "$a" >"$FB/$a"
	chmod +x "$FB/$a"
done
# start_wait LOG [flags...]: rw run --in 10m in the background; sets wpid.
start_wait() {
	local log=$1
	shift
	pushd "$R" >/dev/null
	PATH="$FB:$PATH" "${NEWPG[@]}" "$RW" run --in 10m "$@" "a task that must never start" >"$log" 2>&1 &
	wpid=$!
	popd >/dev/null
}
caff_for() { pgrep -f "caffeinate -i -w $1" >/dev/null; }

start_wait "$OUT/run-in.log"
if wait_file "$OUT/run-in.log" "scheduled:" 30; then
	sleep 1
	pmset -g assertions >"$OUT/pmset-during.txt"
	ps -o pid,ppid,command -p "$(pgrep -d, caffeinate || echo 1)" >"$OUT/caffeinate-ps.txt" 2>&1
	if caff_for "$wpid" && grep -q 'caffeinate' "$OUT/pmset-during.txt" && grep -q 'PreventUserIdleSystemSleep' "$OUT/pmset-during.txt"; then
		pass "while rw run --in waits: caffeinate -i -w <rw pid> runs and pmset lists its PreventUserIdleSystemSleep assertion"
		grep -i caffeinate "$OUT/pmset-during.txt" | head -3 >>"$SUM"
	else
		fail "no caffeinate assertion while rw run --in waits (see pmset-during.txt)"
	fi
	kill -INT "$wpid"
	if wait_gone "$wpid" 15; then pass "Ctrl+C (SIGINT) cancelled the scheduled wait"; else fail "rw run ignored SIGINT"; kill -9 "$wpid"; fi
	sleep 1
	pmset -g assertions >"$OUT/pmset-after.txt"
	if caff_for "$wpid" || grep -q 'caffeinate' "$OUT/pmset-after.txt"; then
		fail "caffeinate still holds the machine awake after rw run was cancelled"
	else
		pass "after the cancel: no caffeinate process, no assertion"
	fi
else
	fail "rw run --in never printed 'scheduled:' (see run-in.log)"
	kill -9 "$wpid" 2>/dev/null
fi

start_wait "$OUT/run-in-kill9.log"
if wait_file "$OUT/run-in-kill9.log" "scheduled:" 30 && sleep 1 && caff_for "$wpid"; then
	kill -9 "$wpid"
	sleep 3
	if caff_for "$wpid"; then fail "caffeinate outlived a killed rw (-w did not work)"; else pass "rw killed hard (SIGKILL): caffeinate -w ended by itself"; fi
else
	fail "second scheduled wait did not start caffeinate (see run-in-kill9.log)"
	kill -9 "$wpid" 2>/dev/null
fi

start_wait "$OUT/run-in-allowsleep.log" --allow-sleep
if wait_file "$OUT/run-in-allowsleep.log" "scheduled:" 30; then
	sleep 1
	if caff_for "$wpid"; then fail "--allow-sleep still started caffeinate"; else pass "--allow-sleep: no caffeinate"; fi
else
	fail "rw run --in --allow-sleep never printed 'scheduled:'"
fi
kill -9 "$wpid" 2>/dev/null

# --- rw app -----------------------------------------------------------------
section "rw app (Chrome app window)"
CH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
if [ ! -x "$CH" ]; then
	info "Google Chrome is not installed: rw app not checked"
else
	info "$("$CH" --version)"
	# The runner's Chrome comes from a Homebrew cask and still carries the
	# quarantine flag, so its first start waits on a Gatekeeper dialog
	# ("downloaded from the Internet") that nobody can click here.
	if xattr -p com.apple.quarantine "/Applications/Google Chrome.app" >/dev/null 2>&1; then
		info "Chrome carries com.apple.quarantine (Homebrew cask): removing it so no Gatekeeper prompt blocks its first start"
		sudo xattr -dr com.apple.quarantine "/Applications/Google Chrome.app"
	fi
	# Skip Chrome's first-run welcome page in the runner's fresh profile.
	mkdir -p "$HOME/Library/Application Support/Google/Chrome"
	touch "$HOME/Library/Application Support/Google/Chrome/First Run"
	printf '<title>OtherWindow</title><h1>Another Chrome window: it must survive rw</h1>\n' >"$T/other.html"
	chrome_running() { pgrep -x "Google Chrome" >/dev/null; }
	# Window titles: System Events (accessibility) first, then Chrome's own
	# AppleScript (automation); either may be blocked by TCC.
	titles() {
		tmo 20 osascript -e 'tell application "System Events" to get name of every window of process "Google Chrome"' 2>/dev/null ||
			tmo 20 osascript -e 'tell application "Google Chrome" to get title of every window' 2>&1
	}
	se_titles() { tmo 20 osascript -e 'tell application "System Events" to get name of every window of process "Google Chrome"' 2>&1; }
	quit_chrome() {
		tmo 20 osascript -e 'quit app "Google Chrome"' >/dev/null 2>&1
		for _ in $(seq 20); do chrome_running || return 0; sleep 1; done
		pkill -x "Google Chrome"
		sleep 2
	}
	# close_app: close the Relayweft window like a user would; prints how.
	close_app() {
		if tmo 20 osascript -e 'tell application "System Events" to tell process "Google Chrome" to click (first button whose subrole is "AXCloseButton") of (first window whose name contains "Relayweft")' >>"$OUT/close.txt" 2>&1; then
			echo "System Events (clicked the window's close button)"
			return 0
		fi
		if tmo 20 osascript -e 'tell application "Google Chrome" to close (every window whose title contains "Relayweft")' >>"$OUT/close.txt" 2>&1; then
			echo "AppleScript to Chrome (close window)"
			return 0
		fi
		return 1
	}

	# A: another Chrome window is open; rw app's window is handed to that
	# Chrome; closing rw's window makes rw exit and leaves the other window.
	tmo 30 open -a "Google Chrome" "file://$T/other.html"
	for _ in $(seq 30); do chrome_running && break; sleep 1; done
	sleep 5
	before=$(titles)
	info "Chrome windows before rw app: $before"
	info "System Events sees: $(se_titles)"
	pushd "$R" >/dev/null
	"${NEWPG[@]}" "$RW" app --demo >"$OUT/app-a.log" 2>&1 &
	apid=$!
	popd >/dev/null
	if ! wait_file "$OUT/app-a.log" "opened in" 30; then
		fail "rw app did not open a window (see app-a.log)"
	else
		info "rw app: $(grep 'opened in' "$OUT/app-a.log")"
		sleep 8
		info "Google Chrome main processes: $(pgrep -x 'Google Chrome' | wc -l | tr -d ' ') (rw's open -n instance hands the window over and exits)"
		shot app-a-open
		t=$(titles)
		info "Chrome windows with rw app open: $t"
		if echo "$t" | grep -q Relayweft; then pass "rw app opened a Chrome window titled Relayweft"; else info "could not read the Relayweft window title (Apple Events blocked?): $t"; fi
		if how=$(close_app); then
			closed=$(date +%s)
			info "closed the Relayweft window via $how"
			if wait_gone "$apid" 60; then
				took=$(($(date +%s) - closed))
				if [ "$took" -le 15 ]; then
					pass "rw app exited ${took}s after its window was closed (goodbye + 5s grace)"
				else
					fail "rw app exited only ${took}s after its window closed (the 30s no-goodbye fallback, not the goodbye)"
				fi
			else
				fail "rw app still runs 60s after its window was closed"
				kill -INT -- -"$apid"
			fi
		else
			info "could not close the window from a script (no Apple Events / accessibility on this runner): see close.txt"
			kill -INT -- -"$apid"
			wait_gone "$apid" 15
		fi
		sleep 2
		shot app-a-after
		if chrome_running; then pass "Chrome still runs after rw app exited"; else fail "Chrome died with rw app"; fi
		t=$(titles)
		info "Chrome windows after: $t"
		# Every window that was open before rw app must still be there.
		missing=""
		while IFS= read -r w; do
			[ -n "$w" ] && ! echo "$t" | grep -qF "$w" && missing="$missing[$w]"
		done <<<"$(echo "$before" | tr ',' '\n' | sed 's/^ *//' | grep -v Relayweft)"
		if echo "$t" | grep -qi 'error'; then
			info "window titles not readable"
		elif [ -z "$missing" ] && [ -n "$before" ]; then
			pass "the other Chrome window(s) survived: $before"
		else
			fail "Chrome windows gone after rw app exited: $missing"
		fi
	fi
	kill -9 "$apid" 2>/dev/null

	# B: Chrome is not running, so rw app starts it. Ctrl+C / closing the
	# terminal signals rw's whole process group; Chrome must survive it
	# (the Windows bug: closing rw killed every Edge window).
	for sig in INT HUP; do
		quit_chrome
		pushd "$R" >/dev/null
		"${NEWPG[@]}" "$RW" app --demo >"$OUT/app-b-$sig.log" 2>&1 &
		bpid=$!
		popd >/dev/null
		if ! wait_file "$OUT/app-b-$sig.log" "opened in" 30; then
			fail "rw app ($sig case) did not open a window"
			kill -9 "$bpid" 2>/dev/null
			continue
		fi
		for _ in $(seq 30); do chrome_running && break; sleep 1; done
		sleep 6
		tmo 30 open -a "Google Chrome" "file://$T/other.html"
		sleep 4
		info "Chrome windows ($sig case, before): $(titles)"
		shot "app-b-$sig-open"
		kill -"$sig" -- -"$bpid"
		if wait_gone "$bpid" 20; then pass "SIG$sig to rw app's process group stopped rw"; else fail "rw app ignored SIG$sig"; kill -9 "$bpid"; fi
		sleep 3
		shot "app-b-$sig-after"
		if chrome_running; then
			pass "Chrome (started by rw app) survived SIG$sig to rw's process group"
			info "Chrome windows ($sig case, after): $(titles)"
		else
			fail "SIG$sig to rw app's process group killed Chrome, which rw had started"
		fi
	done
	quit_chrome
fi

# --- rw update --------------------------------------------------------------
section "rw update on macOS"
mkdir -p "$T/upd"
go build -ldflags "-X main.version=0.0.1" -o "$T/upd/rw" ./cmd/rw
"$T/upd/rw" update --yes >"$OUT/update.txt" 2>&1
rc=$?
if [ $rc -ne 0 ] && grep -qi 'rate limit\|403' "$OUT/update.txt"; then
	info "unauthenticated GitHub API rate-limited; retrying with the run's own read-only token"
	GH_TOKEN=${RUN_TOKEN:-} "$T/upd/rw" update --yes >>"$OUT/update.txt" 2>&1
	rc=$?
fi
latest=$(awk '/^latest:/ {print $2}' "$OUT/update.txt" | head -1)
now=$("$T/upd/rw" version 2>&1)
codesign -dv "$T/upd/rw" >"$OUT/update-codesign.txt" 2>&1
xattr -l "$T/upd/rw" >>"$OUT/update-codesign.txt" 2>&1
if [ $rc -eq 0 ] && [ -n "$latest" ] && echo "$now" | grep -q "$latest"; then
	pass "rw update replaced a 0.0.1 build with release $latest; the new binary starts ($now)"
else
	fail "rw update: exit $rc, latest '$latest', new binary says '$now' (see update.txt)"
fi

section "result"
echo "$fails failure(s)" | tee -a "$SUM"
[ "$fails" -eq 0 ]
