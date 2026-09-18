# Raw session: MULTIAPPEND with two non-synchronising literals, then FETCH.
exec 3<>/dev/tcp/imap/143
read -r -u 3 g
m1=$'Subject: draft one\r\n\r\nbody one\r\n'; m2=$'Subject: draft two\r\n\r\nbody two\r\n'
printf 'a1 LOGIN test test\r\n' >&3
printf 'a2 APPEND Drafts (\\Draft) {%d+}\r\n%s {%d+}\r\n%s\r\n' ${#m1} "$m1" ${#m2} "$m2" >&3
printf 'a3 SELECT Drafts\r\na4 FETCH 1:2 (UID BODY.PEEK[HEADER.FIELDS (SUBJECT)] BINARY.SIZE[1])\r\na5 LOGOUT\r\n' >&3
while IFS= read -r -u 3 -t 10 line; do line=${line%$'\r'}; echo "S: $line"; case "$line" in "a5 "*) break;; esac; done
