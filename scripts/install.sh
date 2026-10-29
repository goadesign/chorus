#!/usr/bin/env sh
# One-line installer for Chorus (https://github.com/goadesign/chorus).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
#
# Environment variables:
#   CHORUS_VERSION   Release tag to install (default: latest release)
#   INSTALL_DIR      Destination directory (default: /usr/local/bin if writable, else ~/.local/bin)
#   CHORUS_NO_SKILL  Set to 1 to skip installing the Chorus agent skill
#   CHORUS_NO_REDIS  Set to 1 to skip checking/installing Redis

set -eu

REPO="goadesign/chorus"
BINARY="chorus"

detect_os() {
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$os" in
		darwin|linux) printf '%s' "$os" ;;
		*)
			printf 'Unsupported operating system: %s\n' "$os" >&2
			exit 1
			;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
		x86_64|amd64) printf 'amd64' ;;
		arm64|aarch64) printf 'arm64' ;;
		*)
			printf 'Unsupported architecture: %s\n' "$arch" >&2
			exit 1
			;;
	esac
}

resolve_version() {
	if [ -n "${CHORUS_VERSION:-}" ]; then
		printf '%s' "$CHORUS_VERSION"
		return
	fi
	tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
	if [ -z "$tag" ]; then
		printf 'Failed to determine latest %s release tag\n' "$REPO" >&2
		exit 1
	fi
	printf '%s' "$tag"
}

choose_install_dir() {
	if [ -n "${INSTALL_DIR:-}" ]; then
		printf '%s' "$INSTALL_DIR"
		return
	fi
	if [ -w "/usr/local/bin" ]; then
		printf '/usr/local/bin'
		return
	fi
	printf '%s/.local/bin' "$HOME"
}

verify_checksum() {
	archive_path="$1"
	archive_name="$2"
	checksums_path="$3"

	expected="$(grep "  ${archive_name}\$" "$checksums_path" | awk '{print $1}')"
	if [ -z "$expected" ]; then
		printf 'Checksum entry for %s not found in checksums.txt\n' "$archive_name" >&2
		exit 1
	fi

	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$archive_path" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
	else
		printf 'Warning: neither sha256sum nor shasum found; skipping checksum verification\n' >&2
		return
	fi

	if [ "$actual" != "$expected" ]; then
		printf 'SHA-256 mismatch for %s: expected %s, got %s\n' "$archive_name" "$expected" "$actual" >&2
		exit 1
	fi
}

ensure_redis() {
	if command -v redis-server >/dev/null 2>&1; then
		printf 'Redis: found redis-server in PATH (chorus will auto-start it if needed)\n'
		return
	fi
	if command -v docker >/dev/null 2>&1; then
		printf 'Redis: found docker in PATH (chorus will auto-start redis:7-alpine if needed)\n'
		return
	fi
	if command -v brew >/dev/null 2>&1; then
		printf 'Installing Redis via Homebrew...\n'
		brew install redis
		return
	fi
	if command -v apt-get >/dev/null 2>&1; then
		if [ "$(id -u)" -eq 0 ]; then
			printf 'Installing redis-server via apt-get...\n'
			apt-get update -y && apt-get install -y redis-server
			return
		elif command -v sudo >/dev/null 2>&1; then
			printf 'Installing redis-server via sudo apt-get...\n'
			sudo apt-get update -y && sudo apt-get install -y redis-server
			return
		fi
	fi
	printf 'Note: install Redis 7.0+ (brew install redis, apt install redis-server, or Docker) before running chorus\n'
}

main() {
	os="$(detect_os)"
	arch="$(detect_arch)"
	version="$(resolve_version)"
	ver_num="${version#v}"
	archive="${BINARY}_${ver_num}_${os}_${arch}.tar.gz"
	base_url="https://github.com/${REPO}/releases/download/${version}"

	tmpdir="$(mktemp -d)"
	trap 'rm -rf "$tmpdir"' EXIT INT TERM

	printf 'Downloading %s %s (%s/%s)...\n' "$BINARY" "$version" "$os" "$arch"
	curl -fsSL "${base_url}/${archive}" -o "${tmpdir}/${archive}"
	curl -fsSL "${base_url}/checksums.txt" -o "${tmpdir}/checksums.txt"

	verify_checksum "${tmpdir}/${archive}" "$archive" "${tmpdir}/checksums.txt"

	tar -xzf "${tmpdir}/${archive}" -C "$tmpdir"
	dest_dir="$(choose_install_dir)"
	mkdir -p "$dest_dir"
	install -m 0755 "${tmpdir}/${BINARY}" "${dest_dir}/${BINARY}"

	printf 'Installed %s %s to %s/%s\n' "$BINARY" "$version" "$dest_dir" "$BINARY"
	if [ "${CHORUS_NO_SKILL:-0}" != "1" ]; then
		"${dest_dir}/${BINARY}" -install-skill
	fi
	if [ "${CHORUS_NO_REDIS:-0}" != "1" ]; then
		ensure_redis
	fi
	case ":${PATH}:" in
		*":${dest_dir}:"*) ;;
		*) printf 'Note: add %s to your PATH: export PATH="%s:$PATH"\n' "$dest_dir" "$dest_dir" ;;
	esac
}

main "$@"
