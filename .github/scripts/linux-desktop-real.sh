#!/bin/bash
# Real-desktop checks on a GitHub Ubuntu runner: a virtual X display (Xvfb)
# with a window manager (fluxbox) and a real notification daemon (dunst) on
# a D-Bus session bus. Run by .github/workflows/macos-real.yml inside
# dbus-run-session; needs no secrets and makes no AI calls.
#
#   OUT=dir SY=path/to/sy dbus-run-session -- .github/scripts/linux-desktop-real.sh
#
# Output as in macos-real.sh: PASS/FAIL/INFO lines in $OUT/summary.txt.
set -u
OUT=${OUT:?}
SY=${SY:?}
T=${RUNNER_TEMP:-/tmp}/sy-real
rm -rf "$T"
mkdir -p "$OUT" "$T"
SUM="$OUT/summary.txt"
: >"$SUM"
fails=0
pass() { echo "PASS: $*" | tee -a "$SUM"; }
fail() { echo "FAIL: $*" | tee -a "$SUM"; fails=$((fails + 1)); }
info() { echo "INFO: $*" | tee -a "$SUM"; }
section() { echo | tee -a "$SUM"; echo "== $* ==" | tee -a "$SUM"; }
shot() { import -window root "$OUT/$1.png" 2>/dev/null && info "screenshot $1.png"; }
wait_file() {
	for _ in $(seq "$3"); do
		grep -q "$2" "$1" 2>/dev/null && return 0
		sleep 1
	done
	return 1
}
wait_gone() {
	for _ in $(seq "$2"); do
		kill -0 "$1" 2>/dev/null || return 0
		sleep 1
	done
	return 1
}

export DISPLAY=:99
Xvfb :99 -screen 0 1280x800x24 >"$OUT/xvfb.log" 2>&1 &
sleep 2
fluxbox >"$OUT/fluxbox.log" 2>&1 &
dunst >"$OUT/dunst.log" 2>&1 &
sleep 2
dbus-monitor --session "type='method_call',interface='org.freedesktop.Notifications'" >"$OUT/dbus-notify.txt" 2>&1 &
mon=$!
sleep 1

section "machine"
info "$(. /etc/os-release && echo "$PRETTY_NAME"), $(uname -r)"
info "sy $("$SY" version)"
info "notify-send: $(notify-send --version 2>&1), dunst: $(dunst --version 2>&1 | head -1)"

# --- notifications ----------------------------------------------------------
section "desktop notifications (internal/notify via notify-send)"
go test -c -o "$T/notify.test" ./internal/notify || fail "build notify test"
SY_REAL_DESKTOP=1 "$T/notify.test" -test.run 'TestRealDesktopToast$' -test.v >"$OUT/notify-test.txt" 2>&1
rc=$?
sleep 1
shot linux-notify
marker=$(grep -o 'marker sytest[0-9]*' "$OUT/notify-test.txt" | head -1 | cut -d' ' -f2)
sent=$(grep -c ': sent in ' "$OUT/notify-test.txt")
if [ $rc -eq 0 ] && [ "$sent" -eq 7 ]; then
	pass "notify.Send ran notify-send without error for all 7 test notifications (marker $marker)"
else
	fail "notify.Send: test exit $rc, $sent of 7 sent (see notify-test.txt)"
fi
sleep 1
kill "$mon"
calls=$(grep -c 'member=Notify' "$OUT/dbus-notify.txt")
withmarker=$(grep -c "$marker" "$OUT/dbus-notify.txt")
if [ "$calls" -ge 7 ] && [ "$withmarker" -ge 7 ]; then
	pass "the session bus carried $calls Notify calls to org.freedesktop.Notifications, $withmarker with the marker"
else
	fail "D-Bus shows $calls Notify calls, $withmarker with the marker (see dbus-notify.txt)"
fi
dunstctl history >"$OUT/dunst-history.json" 2>&1
n=$(($(dunstctl count displayed) + $(dunstctl count waiting) + $(dunstctl count history)))
if [ "$n" -ge 7 ]; then pass "dunst holds $n notifications (displayed + waiting + history)"; else fail "dunst holds only $n notifications"; fi
info "app name on the bus: $(grep -A2 'member=Notify' "$OUT/dbus-notify.txt" | grep -m1 -o '"Switchyard"')"

# --- sy app -----------------------------------------------------------------
section "sy app (Chrome app window)"
if ! command -v google-chrome >/dev/null; then
	info "google-chrome is not installed: sy app not checked"
else
	info "$(google-chrome --version)"
	mkdir -p "$HOME/.config/google-chrome"
	touch "$HOME/.config/google-chrome/First Run"
	printf '<title>OtherWindow</title><h1>Another Chrome window: it must survive sy</h1>\n' >"$T/other.html"
	R="$T/repo"
	mkdir -p "$R"
	chrome_running() { pgrep -x chrome >/dev/null; }
	titles() { wmctrl -l | cut -d' ' -f5- | tr '\n' '|'; }
	quit_chrome() {
		pkill -TERM -x chrome
		for _ in $(seq 20); do chrome_running || return 0; sleep 1; done
		pkill -KILL -x chrome
		sleep 1
	}

	# A: another Chrome window is open; closing sy's window makes sy exit
	# and leaves the other window.
	setsid google-chrome --new-window "file://$T/other.html" >"$OUT/chrome-other.log" 2>&1 &
	for _ in $(seq 30); do chrome_running && break; sleep 1; done
	sleep 5
	info "windows before sy app: $(titles)"
	(cd "$R" && exec setsid "$SY" app --demo) >"$OUT/app-a.log" 2>&1 &
	apid=$!
	if ! wait_file "$OUT/app-a.log" "opened in" 30; then
		fail "sy app did not open a window (see app-a.log)"
	else
		info "sy app: $(grep 'opened in' "$OUT/app-a.log")"
		sleep 8
		shot linux-app-a-open
		t=$(titles)
		info "windows with sy app open: $t"
		if echo "$t" | grep -q Switchyard; then pass "sy app opened a window titled Switchyard"; else fail "no Switchyard window"; fi
		wmctrl -c Switchyard
		closed=$(date +%s)
		if wait_gone "$apid" 60; then
			took=$(($(date +%s) - closed))
			if [ "$took" -le 15 ]; then pass "sy app exited ${took}s after its window was closed"; else fail "sy app exited only ${took}s after its window closed"; fi
		else
			fail "sy app still runs 60s after its window was closed"
			kill -INT -- -"$apid"
		fi
		sleep 2
		shot linux-app-a-after
		if chrome_running; then pass "Chrome still runs after sy app exited"; else fail "Chrome died with sy app"; fi
		t=$(titles)
		info "windows after: $t"
		if echo "$t" | grep -q OtherWindow; then pass "the other Chrome window survived"; else fail "the other Chrome window is gone"; fi
	fi
	kill -9 "$apid" 2>/dev/null

	# B: Chrome is not running, so sy app starts it. Ctrl+C or closing the
	# terminal signals sy's whole process group; Chrome must survive.
	for sig in INT HUP; do
		quit_chrome
		(cd "$R" && exec setsid "$SY" app --demo) >"$OUT/app-b-$sig.log" 2>&1 &
		bpid=$!
		if ! wait_file "$OUT/app-b-$sig.log" "opened in" 30; then
			fail "sy app ($sig case) did not open a window"
			kill -9 "$bpid" 2>/dev/null
			continue
		fi
		for _ in $(seq 30); do chrome_running && break; sleep 1; done
		sleep 6
		setsid google-chrome --new-window "file://$T/other.html" >>"$OUT/chrome-other.log" 2>&1 &
		sleep 4
		info "windows ($sig case, before): $(titles)"
		shot "linux-app-b-$sig-open"
		kill -"$sig" -- -"$bpid"
		if wait_gone "$bpid" 20; then pass "SIG$sig to sy app's process group stopped sy"; else fail "sy app ignored SIG$sig"; kill -9 "$bpid"; fi
		sleep 3
		shot "linux-app-b-$sig-after"
		if chrome_running && titles | grep -q OtherWindow; then
			pass "Chrome (started by sy app) and its other window survived SIG$sig to sy's process group"
		else
			fail "SIG$sig to sy app's process group killed Chrome, which sy had started (windows now: $(titles))"
		fi
	done
	quit_chrome
fi

section "result"
echo "$fails failure(s)" | tee -a "$SUM"
[ "$fails" -eq 0 ]
