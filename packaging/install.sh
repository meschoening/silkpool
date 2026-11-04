#!/bin/sh
# Installs Silkpool from the release this copy was published with
# (ADR 0013). Run it as root:
#
#   curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<version>/install.sh | sudo sh
#   curl -fsSL …/install.sh | sudo sh -s -- <enrollment code>
#   curl -fsSL …/install.sh | sudo sh -s -- --hub
#
# On Linux it downloads the release's .deb or .rpm for this machine,
# checks it against the release's SHA256SUMS, and installs it with
# apt-get, dnf or yum. Given an enrollment code, it then enrolls the
# machine. With --hub it installs the Hub's package instead of the
# node's. An upgrade is the same command for the newer release, without
# a code.
#
# Everything runs from main, on the last lines, so a download cut short
# runs nothing.
set -eu

# The release's build fills these in.
version='@VERSION@'
deb_version='@DEB_VERSION@'
rpm_version='@RPM_VERSION@'

release="https://github.com/meschoening/silkpool/releases/download/v$version"
os_release=/etc/os-release

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

usage() {
	echo "usage: install.sh [<enrollment code> | --hub]"
}

# read_os_release prints ID, ID_LIKE, VERSION_ID and PRETTY_NAME from
# os-release, one per line.
read_os_release() {
	(
		ID='' ID_LIKE='' VERSION_ID='' PRETTY_NAME=''
		. "$os_release"
		printf '%s\n%s\n%s\n%s\n' "$ID" "$ID_LIKE" "$VERSION_ID" "${PRETTY_NAME:-$ID}"
	)
}

# package_format prints deb or rpm for a distribution, from its ID and
# ID_LIKE, or nothing if it takes neither package.
package_format() {
	for word in $1 $2; do
		case "$word" in
		debian | ubuntu)
			echo deb
			return
			;;
		fedora | rhel | centos)
			echo rpm
			return
			;;
		esac
	done
}

# package_arch prints the architecture of the packages for a machine,
# from uname -m, or nothing if there are none.
package_arch() {
	case "$1" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	esac
}

# file_name prints the file name of a package in a format for an
# architecture.
file_name() {
	case "$2" in
	deb) echo "${1}_${deb_version}_$3.deb" ;;
	rpm)
		case "$3" in
		amd64) echo "$1-$rpm_version.x86_64.rpm" ;;
		arm64) echo "$1-$rpm_version.aarch64.rpm" ;;
		esac
		;;
	esac
}

# tested reports whether Silkpool is tested on a distribution, from its
# ID and VERSION_ID: Ubuntu 26.04 LTS and later.
tested() {
	[ "$1" = ubuntu ] || return 1
	major=${2%%.*}
	minor=${2#*.}
	minor=${minor#0}
	[ "$major" -gt 26 ] 2>/dev/null && return 0
	[ "$major" -eq 26 ] 2>/dev/null && [ "${minor:-0}" -ge 4 ] 2>/dev/null
}

# download downloads a file of the release into $tmp.
download() {
	curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
		--output "$tmp/$1" "$release/$1" ||
		fail "couldn't download $release/$1"
}

# check fails unless a downloaded file matches its line in the release's
# SHA256SUMS.
check() {
	want=$(awk -v f="$1" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || fail "the release's SHA256SUMS has no line for $1, so it wasn't installed"
	[ "$(echo "$want" | wc -l)" -eq 1 ] || fail "the release's SHA256SUMS has more than one line for $1, so it wasn't installed"
	got=$(sha256sum "$tmp/$1" | cut -d ' ' -f 1)
	[ "$got" = "$want" ] || fail "$1 doesn't match its SHA-256 in the release's SHA256SUMS, so it wasn't installed"
}

main() {
	hub='' code=''
	for arg in "$@"; do
		case "$arg" in
		--hub) hub=1 ;;
		-h | --help)
			usage
			return 0
			;;
		-*)
			usage >&2
			exit 2
			;;
		*)
			if [ -n "$code" ]; then
				usage >&2
				exit 2
			fi
			code=$arg
			;;
		esac
	done
	if [ -n "$hub" ] && [ -n "$code" ]; then
		fail "the Hub isn't enrolled in anything, so --hub takes no enrollment code"
	fi
	case "$version" in
	@*) fail "this copy isn't from a release: download install.sh from one, at https://github.com/meschoening/silkpool/releases" ;;
	esac
	[ "$(uname -s)" = Linux ] || fail "this install.sh installs Silkpool on Linux only"
	[ "$(id -u)" = 0 ] || fail "it installs a package, so run it as root: curl -fsSL $release/install.sh | sudo sh"
	command -v curl >/dev/null || fail "it downloads with curl, which isn't installed"

	package=silkpool
	if [ -n "$hub" ]; then
		package=silkpool-hub
	fi
	[ -r "$os_release" ] || fail "can't read $os_release to tell which package this machine takes"
	{
		read -r id
		read -r id_like
		read -r version_id
		read -r pretty
	} <<EOF
$(read_os_release)
EOF
	format=$(package_format "$id" "$id_like")
	[ -n "$format" ] || fail "Silkpool's Linux packages are .deb and .rpm files, for apt-get, dnf or yum, and $pretty takes neither"
	machine=$(uname -m)
	arch=$(package_arch "$machine")
	[ -n "$arch" ] || fail "Silkpool's Linux packages are for x86_64 and aarch64 machines, not $machine"
	case "$format" in
	deb)
		command -v apt-get >/dev/null || fail "$pretty takes .deb packages, but apt-get isn't installed"
		installer='apt-get install --yes'
		;;
	rpm)
		if command -v dnf >/dev/null; then
			installer='dnf install --assumeyes'
		elif command -v yum >/dev/null; then
			installer='yum install --assumeyes'
		else
			fail "$pretty takes .rpm packages, but neither dnf nor yum is installed"
		fi
		;;
	esac
	if [ "$format" = rpm ]; then
		echo "Silkpool's .rpm packages are untested: Silkpool is tested on Ubuntu 26.04 LTS and later."
	elif ! tested "$id" "$version_id"; then
		echo "$pretty is untested: Silkpool is tested on Ubuntu 26.04 LTS and later."
	fi

	file=$(file_name "$package" "$format" "$arch")
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM
	# apt-get reads the package as its own unprivileged user.
	chmod 755 "$tmp"
	echo "Downloading $file from $release"
	download "$file"
	download SHA256SUMS
	check "$file"
	echo "Installing $file with ${installer%% *}"
	$installer "$tmp/$file" </dev/null
	if [ -n "$code" ]; then
		silkpool enroll "$code" </dev/null
	fi
}

# install.sh's tests source it with SILKPOOL_INSTALL_TEST set, and call
# main themselves.
if [ -z "${SILKPOOL_INSTALL_TEST:-}" ]; then
	main "$@"
fi
