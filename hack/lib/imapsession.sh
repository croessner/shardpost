# Scripted IMAP session: reads commands from stdin (one per line), tags
# them, waits for each tagged reply and prints the dialogue.
exec 3<>/dev/tcp/imap/143
read -r -u 3 greeting
n=0
while IFS= read -r cmd; do
  n=$((n+1)); tag="a$n"
  printf '%s %s\r\n' "$tag" "$cmd" >&3
  echo "C: $cmd"
  while IFS= read -r -u 3 line; do
    line=${line%$'\r'}
    echo "S: $line"
    case "$line" in "$tag "*) break;; esac
  done
done
