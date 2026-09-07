# Activates this checkout's dbarenactl for the current shell session, similar
# to Python's `source venv/bin/activate` or nvm's shell integration.
#
# Must be SOURCED, not executed — a script run as a subprocess can't change
# your shell's PATH. Running it directly (./scripts/use.sh) will silently
# do nothing useful once the subshell exits.
#
#   source scripts/use.sh
#   . scripts/use.sh
#
# After sourcing, `dbarenactl` resolves from any directory to a wrapper that
# rebuilds this checkout before every invocation (see scripts/shim/dbarenactl),
# so it always reflects the current source, including uncommitted changes.

root="$(cd "$(dirname "${BASH_SOURCE:-$0}")/.." && pwd)"

case ":$PATH:" in
  *":$root/scripts/shim:"*) ;;
  *) export PATH="$root/scripts/shim:$PATH" ;;
esac

echo "dbarenactl: using checkout at $root (rebuilds automatically on every invocation)" >&2
