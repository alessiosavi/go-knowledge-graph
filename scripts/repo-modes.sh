#!/bin/bash
# Git inherits the deploy's private umask. Restore only index-owned paths before
# Compose bind-mounts them into containers running as another user.
set -euo pipefail
[ "$(git rev-parse --is-inside-work-tree 2>/dev/null)" = true ] || exit 0
cd "$(git rev-parse --show-toplevel)"

# GNU and BSD stat use different format flags. Neither follows symlinks here.
if stat -c %a . >/dev/null 2>&1; then
  mode_of() { stat -c %a "$1"; }
else
  mode_of() { stat -f %Lp "$1"; }
fi
fix_mode() {
  local actual
  actual=$(mode_of "$2") || exit 1
  [ "$actual" = "$1" ] || chmod "$1" "$2" || exit 1
}
fixed_dirs=()
fix_dirs() {
  local fixed
  for fixed in ${fixed_dirs[@]+"${fixed_dirs[@]}"}; do
    [ "$fixed" != "$1" ] || return 0
  done
  # Walk from the root down, and never traverse a locally substituted symlink.
  [ "$1" = . ] || fix_dirs "${1%/*}" || return 1
  [ ! -L "$1" ] && [ -d "$1" ] || return 1
  fix_mode 755 "$1"
  fixed_dirs+=("$1")
}

# -z preserves tabs/newlines as well as spaces; the first tab ends the header.
git ls-files --stage -z | while IFS= read -r -d '' entry; do
  case "${entry%% *}" in
    100755) mode=755 ;;
    100644) mode=644 ;;
    *) continue ;; # Symlinks and gitlinks have no regular-file mode to restore.
  esac
  path=./${entry#*$'\t'}
  fix_dirs "${path%/*}" || continue
  if [ ! -L "$path" ] && [ -f "$path" ]; then
    fix_mode "$mode" "$path"
  fi
done
