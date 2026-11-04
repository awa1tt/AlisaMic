#!/bin/sh
# passwd.sh set <хеш> | rollback — меняет пароль root для SSH (вход по паролю через dropbear).
# Хеш (SHA-512 crypt) считает AlisaMic на ПК, сам пароль на колонку не передаётся.
ACT="$1"; HASH="$2"
die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }

F=""
for c in /etc/shadow /etc/passwd; do
  if grep -q '^root:' "$c" 2>/dev/null; then F="$c"; break; fi
done
[ -n "$F" ] || die "не нашёл строку root в /etc/shadow и /etc/passwd"
[ -L "$F" ] && F=$(readlink -f "$F")

ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
mount -o remount,rw / || die "не удалось remount rw"
trap 'sync; [ "$WAS_RW" = 1 ] || mount -o remount,ro / 2>/dev/null' EXIT

case "$ACT" in
set)
  case "$HASH" in '$6$'*) ;; *) die "ожидался хеш \$6\$…";; esac
  case "$HASH" in *[!A-Za-z0-9./\$]*) die "в хеше недопустимые символы";; esac
  [ -f "$F.bak-alisamic" ] || cp -p "$F" "$F.bak-alisamic" || die "не удалось сделать бэкап $F"
  H="$HASH" awk 'BEGIN{FS=OFS=":"} $1=="root"{$2=ENVIRON["H"]} {print}' "$F" > /tmp/alisamic-pw.new || die "awk не сработал"
  [ "$(wc -l < /tmp/alisamic-pw.new)" = "$(wc -l < "$F")" ] || die "число строк изменилось, ничего не записано"
  grep -q '^root:\$6\$' /tmp/alisamic-pw.new || die "хеш не попал в файл, ничего не записано"
  cat /tmp/alisamic-pw.new > "$F" || die "не удалось записать $F"
  rm -f /tmp/alisamic-pw.new
  echo "пароль root записан в $F (бэкап: $F.bak-alisamic)"
  echo "RESULT=OK"
  ;;
rollback)
  [ -f "$F.bak-alisamic" ] || die "нет бэкапа $F.bak-alisamic"
  cat "$F.bak-alisamic" > "$F" || die "не удалось восстановить $F"
  echo "пароль восстановлен из бэкапа"
  echo "RESULT=OK"
  ;;
*) die "неизвестное действие";;
esac
