# ManageSieve session over bash /dev/tcp: commands from stdin (PUTSCRIPT
# takes the script from a following heredoc marker "---"), prints the dialogue.
exec 3<>/dev/tcp/imap/4190
while IFS= read -r -u 3 -t 5 line; do line=${line%$'\r'}; echo "S: $line"; [[ $line == OK* ]] && break; done
while IFS= read -r cmd; do
  if [[ $cmd == PUTSCRIPT* ]]; then
    script=""; while IFS= read -r l; do [[ $l == "---" ]] && break; script+="$l"$'\n'; done
    printf '%s {%d+}\r\n%s\r\n' "$cmd" ${#script} "$script" >&3; echo "C: $cmd {${#script}+} ..."
  else
    printf '%s\r\n' "$cmd" >&3; echo "C: $cmd"
  fi
  while IFS= read -r -u 3 -t 5 line; do line=${line%$'\r'}; echo "S: $line"; [[ $line == OK* || $line == NO* || $line == BYE* ]] && break; done
  [[ $cmd == LOGOUT ]] && break
done
