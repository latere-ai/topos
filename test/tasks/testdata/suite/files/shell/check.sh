# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# Passes when out.txt says shell and the log exists. Shell builtins
# only, so the check needs nothing on PATH.
IFS= read -r line < "$1/out.txt" || true
test "$line" = shell || exit 1
test -f "$2" || exit 2
test "$TOPOS_TASK" = files/shell || exit 3
