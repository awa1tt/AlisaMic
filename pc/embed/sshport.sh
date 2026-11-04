#!/bin/sh
# sshport.sh start|rollback|finish <новый_порт>
# start    — правит init-скрипт dropbear и поднимает ВТОРОЙ слушатель на новом порту (старый остаётся работать)
# finish   — останавливает старый слушатель (после успешной проверки входа на новом порту)
# rollback — возвращает init-скрипт и гасит новый слушатель
ACT="$1"; NEW="$2"
die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }
case "$NEW" in ""|*[!0-9]*) die "порт должен быть числом";; esac
[ "$NEW" -ge 1 ] && [ "$NEW" -le 65535 ] || die "порт должен быть от 1 до 65535"

INIT=$(grep -l dropbear /etc/init.d/* 2>/dev/null | head -n 1)
[ -n "$INIT" ] || die "не нашёл init-скрипт с dropbear в /etc/init.d"
OLD=$(grep -o -- '-p [0-9]*' "$INIT" | head -n 1 | cut -d' ' -f2)
[ -n "$OLD" ] || die "в $INIT не нашёл ключ '-p <порт>' — формат нестандартный, автоматически не меняю"

# главный процесс dropbear на порту $1 (у сессий родитель — тоже dropbear)
find_master() {
  PIDS=$(for q in /proc/[0-9]*; do tr '\0' ' ' < "$q/cmdline" 2>/dev/null | grep -q "^[^ ]*dropbear" && echo "${q#/proc/}"; done)
  for p in $PIDS; do
    tr '\0' ' ' < /proc/$p/cmdline | grep -q -- "-p $1 " || continue
    pp=$(awk '/^PPid:/{print $2}' /proc/$p/status)
    case " $(echo $PIDS) " in *" $pp "*) continue;; esac
    echo "$p"; return
  done
}
port_busy() {
  HEX=$(printf '%04X' "$1")
  awk -v h=":$HEX" '$2 ~ h"$" && $4=="0A" {f=1} END{exit !f}' /proc/net/tcp /proc/net/tcp6 2>/dev/null
}

ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
rw()  { mount -o remount,rw / || die "не удалось remount rw"; }
ro()  { sync; [ "$WAS_RW" = 1 ] || mount -o remount,ro / 2>/dev/null; }

case "$ACT" in
start)
  [ "$NEW" = "$OLD" ] && die "порт и так $OLD"
  port_busy "$NEW" && die "порт $NEW уже занят на колонке"
  M=$(find_master "$OLD")
  [ -n "$M" ] || die "не нашёл работающий dropbear на порту $OLD"
  CMD=$(tr '\0' ' ' < /proc/$M/cmdline | sed "s/-p $OLD /-p $NEW /")
  rw
  [ -f "$INIT.bak-alisamic" ] || cp "$INIT" "$INIT.bak-alisamic" || { ro; die "не удалось сделать бэкап $INIT"; }
  sed -e "s/-p $OLD /-p $NEW /" -e "s/-p $OLD\$/-p $NEW/" "$INIT" > /tmp/alisamic-init.new
  grep -q -- "-p $NEW" /tmp/alisamic-init.new || { ro; die "не удалось подставить порт в $INIT"; }
  cat /tmp/alisamic-init.new > "$INIT" || { ro; die "не удалось записать $INIT"; }
  ro
  echo "init-скрипт обновлён: $OLD -> $NEW"
  ( eval "$CMD" ) </dev/null >/dev/null 2>&1 &
  sleep 1
  M2=$(find_master "$NEW")
  [ -n "$M2" ] || die "новый слушатель на порту $NEW не поднялся"
  echo "OLD=$OLD"
  echo "RESULT=OK"
  ;;
finish)
  # $NEW здесь — СТАРЫЙ порт, который нужно закрыть (передаётся клиентом)
  M=$(find_master "$NEW")
  [ -n "$M" ] && kill "$M" && echo "старый слушатель на порту $NEW остановлен"
  echo "RESULT=OK"
  ;;
rollback)
  M=$(find_master "$NEW")
  [ -n "$M" ] && kill "$M"
  if [ -f "$INIT.bak-alisamic" ]; then
    rw
    cat "$INIT.bak-alisamic" > "$INIT"
    ro
    echo "init-скрипт восстановлен из бэкапа"
  fi
  echo "RESULT=OK"
  ;;
*) die "неизвестное действие";;
esac
