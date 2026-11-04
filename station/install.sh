#!/bin/sh
# install.sh <IP_ПК> [порт] — установка/обновление тапа микрофона Алисы (vqe_0 -> UDP на ПК).
# Запускать НА КОЛОНКЕ под root (или через AlisaMic.exe — он делает это сам).
# libvqe_tap.so должен лежать рядом с этим скриптом ИЛИ уже в /data/quasar/micstream/
# Повторный запуск безопасен: обновляет .so и адрес ПК (ALICE_TAP_TARGET).

PCIP="$1"
PORT="${2:-5000}"
D=/data/quasar/micstream
LJ=/system/vendor/quasar/launch.json
LIB=/system/vendor/quasar/libs/libyandex_evil.so
TMP=/tmp/alisamic-launch.new

die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }

[ -z "$PCIP" ] && { echo "Usage: install.sh <IP_ПК> [порт]"; exit 1; }
case "$PCIP" in *[!0-9.]*) die "IP ПК должен быть вида 192.168.0.5";; esac
case "$PORT" in ""|*[!0-9]*) die "порт должен быть числом";; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "порт должен быть от 1 до 65535"
[ -f "$LJ" ] || die "не найден $LJ — это не та прошивка"

echo "[1/5] проверяю прошивку"
[ -f "$LIB" ] || die "нет $LIB — эта прошивка не подходит"
grep -q YandexVqeC_GetOutputChannelData "$LIB" || die "в libyandex_evil.so нет YandexVqeC_GetOutputChannelData — эта прошивка не подходит"

# чужой LD_PRELOAD (например, от другого проекта) не трогаем
if grep -q LD_PRELOAD "$LJ" && ! grep -q libvqe_tap "$LJ"; then
  die "в launch.json уже есть чужой LD_PRELOAD — автоматически не меняю, чтобы ничего не сломать"
fi

echo "[2/5] кладу libvqe_tap.so"
mkdir -p "$D" || die "не удалось создать $D"
SELF=$(dirname "$0")
if [ -f "$SELF/libvqe_tap.so" ]; then
  cp "$SELF/libvqe_tap.so" "$D/libvqe_tap.so.new" && mv "$D/libvqe_tap.so.new" "$D/libvqe_tap.so" || die "не удалось скопировать libvqe_tap.so"
elif [ ! -f "$D/libvqe_tap.so" ]; then
  die "не найден libvqe_tap.so"
fi
chmod 755 "$D/libvqe_tap.so" 2>/dev/null

echo "[3/5] открываю системный раздел на запись"
ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
mount -o remount,rw / || die "не удалось remount rw"
restore_ro() { sync; [ "$WAS_RW" = 1 ] || mount -o remount,ro / 2>/dev/null; }
trap restore_ro EXIT

echo "[4/5] правлю launch.json"
if grep -q libvqe_tap "$LJ"; then
  # уже установлено — обновляем только адрес приёмника
  if grep -q ALICE_TAP_TARGET "$LJ"; then
    sed "s|ALICE_TAP_TARGET=[0-9.:]*|ALICE_TAP_TARGET=$PCIP:$PORT|" "$LJ" > "$TMP" || die "sed не сработал"
    grep -q "ALICE_TAP_TARGET=$PCIP:$PORT" "$TMP" || die "не удалось подставить адрес приёмника, ничего не изменено"
    if cmp -s "$TMP" "$LJ"; then
      echo "адрес приёмника уже $PCIP:$PORT"
    else
      cat "$TMP" > "$LJ" || die "не удалось записать launch.json"
      echo "адрес приёмника обновлён: $PCIP:$PORT"
    fi
  else
    die "в launch.json есть LD_PRELOAD, но нет ALICE_TAP_TARGET — нужна ручная правка"
  fi
else
  [ -f "$LJ.bak-alisamic" ] || cp "$LJ" "$LJ.bak-alisamic" || die "не удалось сделать бэкап launch.json"
  awk -v ip="$PCIP" -v port="$PORT" '
    /"name": "maind"/ { inm=1 }
    inm && !done && /CA_CERTIFICATES/ {
      comma = ($0 ~ /,[ \t]*$/)
      if (comma) {
        print $0
        print "                        \"LD_PRELOAD=/data/quasar/micstream/libvqe_tap.so\","
        print "                        \"ALICE_TAP_TARGET=" ip ":" port "\","
      } else {
        print $0 ","
        print "                        \"LD_PRELOAD=/data/quasar/micstream/libvqe_tap.so\","
        print "                        \"ALICE_TAP_TARGET=" ip ":" port "\""
      }
      done=1
      next
    }
    { print }
  ' "$LJ" > "$TMP"
  [ "$(grep -c LD_PRELOAD "$TMP")" = 1 ] || die "не удалось пропатчить launch.json (не нашёл место вставки), ничего не изменено"
  [ "$(grep -c ALICE_TAP_TARGET "$TMP")" = 1 ] || die "не удалось пропатчить launch.json, ничего не изменено"
  A=$(tr -cd '{[' < "$LJ" | wc -c); B=$(tr -cd '{[' < "$TMP" | wc -c)
  [ "$A" = "$B" ] || die "после правки скобки не сошлись, ничего не изменено"
  cat "$TMP" > "$LJ" || die "не удалось записать launch.json"
  echo "launch.json пропатчен"
fi

echo "[5/5] проверяю результат"
grep -q "ALICE_TAP_TARGET=$PCIP:$PORT" "$LJ" || die "в launch.json не нашёл ALICE_TAP_TARGET=$PCIP:$PORT"
rm -f "$TMP"
echo "Готово. Нужна перезагрузка колонки. Микрофон будет стримиться в udp://$PCIP:$PORT"
echo "RESULT=OK"
