# IDLE on INBOX and report how long the EXISTS notification took.
exec 3<>/dev/tcp/imap/143
read -r -u 3 g
printf 'a1 LOGIN test test\r\na2 SELECT INBOX\r\na3 IDLE\r\n' >&3
start=$(date +%s%N)
while IFS= read -r -u 3 -t 15 line; do
  line=${line%$'\r'}
  case "$line" in
    "+ "*) echo "S: $line (idling)";;
    "* "*EXISTS) now=$(date +%s%N); echo "S: $line  after $(( (now - start) / 1000000 )) ms"; printf 'DONE\r\na4 LOGOUT\r\n' >&3;;
    "a4 "*) break;;
  esac
done
