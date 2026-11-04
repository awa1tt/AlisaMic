#!/bin/sh
# Печатает key=value о состоянии колонки (читает, ничего не меняет)
D=/data/quasar/micstream
LJ=/system/vendor/quasar/launch.json
LIB=/system/vendor/quasar/libs/libyandex_evil.so

echo "uid=$(id -u 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "kernel=$(uname -r 2>/dev/null)"
echo "uptime=$(cut -d. -f1 /proc/uptime 2>/dev/null)"
if [ -f "$LIB" ]; then
  echo "evil_lib=1"
  if grep -q YandexVqeC_GetOutputChannelData "$LIB"; then echo "vqe_symbol=1"; else echo "vqe_symbol=0"; fi
else
  echo "evil_lib=0"; echo "vqe_symbol=0"
fi
if [ -f "$LJ" ]; then echo "launch_json=1"; else echo "launch_json=0"; fi
if [ -f "$D/libvqe_tap.so" ]; then
  echo "so=1"
  echo "so_md5=$(md5sum "$D/libvqe_tap.so" 2>/dev/null | cut -d' ' -f1)"
  echo "so_size=$(wc -c < "$D/libvqe_tap.so" 2>/dev/null | tr -d ' ')"
else
  echo "so=0"
fi
if grep -q libvqe_tap "$LJ" 2>/dev/null; then echo "patched=1"; else echo "patched=0"; fi
if grep -q LD_PRELOAD "$LJ" 2>/dev/null; then echo "any_preload=1"; else echo "any_preload=0"; fi
echo "target=$(grep -o 'ALICE_TAP_TARGET=[0-9.:]*' "$LJ" 2>/dev/null | head -n 1 | cut -d= -f2)"
if [ -f "$LJ.bak-alisamic" ]; then echo "backup=1"; else echo "backup=0"; fi

running=0; loaded=0
for p in /proc/[0-9]*; do
  if tr '\0' ' ' < "$p/cmdline" 2>/dev/null | grep -q '[m]aind'; then
    running=1
    if grep -q libvqe_tap "$p/maps" 2>/dev/null; then loaded=1; fi
  fi
done
echo "maind=$running"
echo "loaded=$loaded"

echo "rootfs=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1 | cut -d, -f1)"
echo "data_free_kb=$(df -k /data 2>/dev/null | awk 'NR==2{print $4}')"
echo "cards=$(grep -c '\[' /proc/asound/cards 2>/dev/null)"

SSHPID=""
for p in /proc/[0-9]*; do
  if tr '\0' ' ' < "$p/cmdline" 2>/dev/null | grep -q '^[^ ]*dropbear'; then SSHPID="${p#/proc/}"; break; fi
done
if [ -n "$SSHPID" ]; then
  echo "ssh_cmd=$(tr '\0' ' ' < /proc/$SSHPID/cmdline 2>/dev/null)"
fi
INIT=$(grep -l dropbear /etc/init.d/* 2>/dev/null | head -n 1)
echo "ssh_init=$INIT"
[ -n "$INIT" ] && echo "ssh_init_port=$(grep -o -- '-p [0-9]*' "$INIT" 2>/dev/null | head -n 1 | cut -d' ' -f2)"
echo "END=1"
