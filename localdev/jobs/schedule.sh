#!/bin/sh
# Runs `jobs aggregate` every minute and `jobs expire` every hour, starting with both. A failed pass
# is logged by the job itself and runs again on the next tick. (The binary is named by its path:
# `jobs` alone is the shell's job-control builtin.)
trap 'exit 0' TERM INT
minute=0
while :; do
  /usr/local/bin/jobs aggregate
  if [ $((minute % 60)) -eq 0 ]; then
    /usr/local/bin/jobs expire
  fi
  minute=$((minute + 1))
  sleep 60 &
  wait $!
done
