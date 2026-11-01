#!/usr/bin/env bash
#
# Convert one or more epub files into audiobooks with the converter docker
# image, and write the result next to the epub (or into --output).
#
#   ./convert-epub.sh ~/Books/"The Book.epub"
#
# The container is started for this call only, its log is shown while the book
# is spoken, and the script exits as soon as every book given to it is
# converted or has failed.
set -euo pipefail

image="${ABC_IMAGE:-audiobookconverter:local}"
output="$PWD"
cache_volume="${ABC_CACHE_VOLUME:-audiobookconverter-models}"
voice=""
bitrate=""
speed=""
no_m4b=false
keep_patterns=""
extra_skip_patterns=""
attempts=1
timeout=0
build=false
dry_run=false
epubs=()

usage() {
	cat <<EOF
Usage: ${0##*/} [OPTION]... FILE.epub...

Options:
  -o, --output DIR       folder the audiobooks are written to (default: the
                         current folder; each book gets its own subfolder)
  -i, --image IMAGE      converter image (default: \$ABC_IMAGE or
                         $image)
      --build            build the image from the repository of this script
                         before converting
  -c, --cache-volume NAME
                         docker volume that keeps the speech model between
                         runs (default: $cache_volume; without it every
                         run downloads the model again)
  -v, --voice VOICE      piper voice, e.g. bm_lewis (default: the image's)
  -b, --bitrate BITRATE  mp3 bitrate, e.g. 128k (default: the image's)
  -s, --speed FACT       speech rate between 0.5 and 2.0
      --no-m4b           keep the chapter mp3 files only, do not build the
                         single .m4b file
      --keep-regex RE    rescue chapters whose title matches a skip pattern
      --skip-regex RE    drop more chapters, in addition to the defaults
  -a, --attempts N       conversion attempts per book (default: 1)
  -t, --timeout SECONDS  give up after this many seconds (default: wait)
  -n, --dry-run          print the docker command and exit
  -h, --help             this text

The files are written by the user of the container (root), so they may end up
owned by root on the host.
EOF
}

die() {
	printf '%s: %s\n' "${0##*/}" "$1" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	-o | --output) [[ $# -ge 2 ]] || die "$1 needs an argument"; output="$2"; shift 2 ;;
	-i | --image) [[ $# -ge 2 ]] || die "$1 needs an argument"; image="$2"; shift 2 ;;
	--build) build=true; shift ;;
	-c | --cache-volume) [[ $# -ge 2 ]] || die "$1 needs an argument"; cache_volume="$2"; shift 2 ;;
	-v | --voice) [[ $# -ge 2 ]] || die "$1 needs an argument"; voice="$2"; shift 2 ;;
	-b | --bitrate) [[ $# -ge 2 ]] || die "$1 needs an argument"; bitrate="$2"; shift 2 ;;
	-s | --speed) [[ $# -ge 2 ]] || die "$1 needs an argument"; speed="$2"; shift 2 ;;
	--no-m4b) no_m4b=true; shift ;;
	--keep-regex) [[ $# -ge 2 ]] || die "$1 needs an argument"; keep_patterns="$2"; shift 2 ;;
	--skip-regex) [[ $# -ge 2 ]] || die "$1 needs an argument"; extra_skip_patterns="$2"; shift 2 ;;
	-a | --attempts) [[ $# -ge 2 ]] || die "$1 needs an argument"; attempts="$2"; shift 2 ;;
	-t | --timeout) [[ $# -ge 2 ]] || die "$1 needs an argument"; timeout="$2"; shift 2 ;;
	-n | --dry-run) dry_run=true; shift ;;
	-h | --help) usage; exit 0 ;;
	--) shift; while [[ $# -gt 0 ]]; do epubs+=("$1"); shift; done ;;
	-*) die "unknown option $1 (try --help)" ;;
	*) epubs+=("$1"); shift ;;
	esac
done

[[ ${#epubs[@]} -gt 0 ]] || die "no epub given (try --help)"
[[ "$attempts" =~ ^[0-9]+$ ]] || die "--attempts must be a number"
[[ "$timeout" =~ ^[0-9]+$ ]] || die "--timeout must be a number of seconds"

for f in "${epubs[@]}"; do
	[[ -f "$f" ]] || die "no such file: $f"
	case "${f,,}" in *.epub) ;; *) die "not an epub: $f" ;; esac
done

command -v docker >/dev/null || die "docker is not installed"

# Absolute paths: the epub folders are mounted into the container.
abs=()
for f in "${epubs[@]}"; do abs+=("$(readlink -f -- "$f")"); done

mkdir -p -- "$output"
output="$(readlink -f -- "$output")"

if $build; then
	repo="$(cd -- "$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")" && pwd)"
	docker build -t "$image" "$repo"
elif ! docker image inspect "$image" >/dev/null 2>&1; then
	die "image $image not found: build it with '--build', or 'docker build -t $image .', or point --image at one"
fi

# A book counts as converted when its epub sits next to its audio; such a book
# is never converted again, so asking for it is not an error but a no-op.
pending=()
for f in "${abs[@]}"; do
	name="$(basename -- "$f")"
	marker="$output/${name%.epub}/$name"
	if [[ -e "$marker" ]]; then
		printf "'%s' is already converted (delete '%s' to do it again)\n" "$name" "$marker"
	else
		pending+=("$f")
	fi
done
if [[ ${#pending[@]} -eq 0 ]]; then
	exit 0
fi

run=(docker run -d --label audiobookconverter.one-shot=1
	-v "$output:/audiobooks"
	-v "$cache_volume:/root/.cache"
	-e CONVERTER_IN=/books
	-e CONVERTER_OUT=/audiobooks
	-e CONVERTER_MAX_ATTEMPTS="$attempts"
	-e CONVERTER_POLL_MINUTES=1)
i=1
for f in "${pending[@]}"; do
	run+=(-v "$(dirname -- "$f"):/books/$i:ro")
	i=$((i + 1))
done
if [[ -n "$voice" ]]; then run+=(-e CONVERTER_VOICE="$voice"); fi
if [[ -n "$bitrate" ]]; then run+=(-e CONVERTER_BITRATE="$bitrate"); fi
if [[ -n "$speed" ]]; then run+=(-e CONVERTER_SPEED="$speed"); fi
if [[ -n "$keep_patterns" ]]; then run+=(-e CONVERTER_KEEP_PATTERNS="$keep_patterns"); fi
if [[ -n "$extra_skip_patterns" ]]; then run+=(-e CONVERTER_EXTRA_SKIP_PATTERNS="$extra_skip_patterns"); fi
if $no_m4b; then run+=(-e CONVERTER_M4B=false); fi
run+=("$image" /opt/audiobookconverter)

if $dry_run; then
	printf '%q ' "${run[@]}"
	printf '\n'
	exit 0
fi

container="$("${run[@]}")"
name="audiobookconverter-$$-${RANDOM}"
docker rename "$container" "$name" >/dev/null 2>&1 || name="$container"

logfile="$(mktemp)"
logpid=""
cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	if [[ -n "$logpid" ]]; then kill "$logpid" >/dev/null 2>&1 || true; fi
}
trap 'cleanup; rm -f "$logfile"; exit 130' INT TERM

docker logs --follow "$name" >"$logfile" 2>&1 &
logpid=$!

printf 'Converting %s into %s (log: docker logs %s)\n\n' \
	"$(printf '%s, ' "${pending[@]##*/}" | sed 's/, $//')" "$output" "$name"

declare -A result=()
offset=0
status=0
while :; do
	size="$(wc -c <"$logfile")"
	if ((size > offset)); then
		tail -c +"$((offset + 1))" "$logfile"
		offset="$size"
	fi

	for f in "${pending[@]}"; do
		book="$(basename -- "$f")"
		[[ -n "${result[$book]:-}" ]] && continue
		if grep -qF "Finished book '$book'" "$logfile"; then
			result[$book]=ok
		elif grep -qF "Book '$book' failed:" "$logfile" ||
			grep -qF "Skipping '$book': already failed" "$logfile"; then
			result[$book]=failed
		fi
	done

	unresolved=0
	for f in "${pending[@]}"; do
		[[ -n "${result[$(basename -- "$f")]:-}" ]] || unresolved=1
	done
	((unresolved == 0)) && break

	if [[ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null || echo false)" != true ]]; then
		break
	fi
	if ((timeout > 0 && SECONDS >= timeout)); then
		printf '\nGiving up after %ss; the books given are not marked as converted.\n' "$timeout" >&2
		status=1
		break
	fi
	sleep 2
done

cleanup

printf '\n'
for f in "${pending[@]}"; do
	book="$(basename -- "$f")"
	case "${result[$book]:-not finished}" in
	ok)
		printf '%s:\n' "$book"
		find "$output/${book%.epub}" -maxdepth 1 -type f \
			\( -name '*.mp3' -o -name '*.m4b' \) -printf '  %f\n' | sort || true
		;;
	failed)
		printf '%s: failed\n' "$book"
		grep -F "Book '$book' failed:" "$logfile" | tail -1 | sed 's/.*failed: /  /' >&2 || true
		printf '  It is not marked as converted, so it will be tried again.\n' >&2
		status=1
		;;
	*)
		printf '%s: not finished\n' "$book" >&2
		tail -3 "$logfile" | sed 's/^/  /' >&2 || true
		status=1
		;;
	esac
done
rm -f "$logfile"
exit "$status"
