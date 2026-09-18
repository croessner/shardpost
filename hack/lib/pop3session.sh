# Scripted POP3 session over bash /dev/tcp: commands from stdin, prints
# the dialogue, reads multi-line responses up to the "." terminator.
exec 3<>/dev/tcp/imap/110
read -r -u 3 g; echo "S: ${g%$'\r'}"
while IFS= read -r cmd; do
  printf '%s\r\n' "$cmd" >&3
  echo "C: $cmd"
  IFS= read -r -u 3 line; line=${line%$'\r'}; echo "S: $line"
  case "$cmd" in
    CAPA|LIST|UIDL|RETR*|TOP*) [[ $line == +OK* ]] && while IFS= read -r -u 3 l; do l=${l%$'\r'}; echo "   $l"; [[ $l == "." ]] && break; done;;
  esac
  [[ $cmd == QUIT ]] && break
done
