#!/bin/sh
# install.sh <IP_ПК> [порт] — установка/обновление тапа микрофона Алисы (vqe_0 -> UDP на ПК).
# Запускать НА КОЛОНКЕ под root (или через AlisaMic.exe — он делает это сам).
# libvqe_tap.so может лежать рядом с этим скриптом, в $EMBEDDED_SO_B64, или уже в /data/quasar/micstream/
# Повторный запуск безопасен: обновляет .so и адрес ПК (ALICE_TAP_TARGET).

PCIP="$1"
PORT="${2:-5000}"
D=/data/quasar/micstream
LJ=/system/vendor/quasar/launch.json
BAK=/system/vendor/quasar/launch.json.bak-alisamic
BAK_DATA=/data/quasar/launch.json.bak-alisamic
LIB=/system/vendor/quasar/libs/libyandex_evil.so
TMP=/tmp/alisamic-launch.new

die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }

[ -z "$PCIP" ] && { echo "Usage: install.sh <IP_ПК> [порт]"; exit 1; }
case "$PCIP" in *[!0-9.]*) die "IP ПК должен быть вида 192.168.0.5";; esac
case "$PORT" in ""|*[!0-9]*) die "порт должен быть числом";; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "порт должен быть от 1 до 65535"
[ -f "$LJ" ] || die "не найден $LJ — это не та прошивка"

echo "[STEP:fw:start] Проверка прошивки и совместимости VQE"
[ -f "$LIB" ] || { echo "[STEP:fw:fail] нет $LIB"; die "нет $LIB — эта прошивка не подходит"; }
grep -q YandexVqeC_GetOutputChannelData "$LIB" || { echo "[STEP:fw:fail] VQE символ не найден"; die "в libyandex_evil.so нет YandexVqeC_GetOutputChannelData — эта прошивка не подходит"; }

if grep -q LD_PRELOAD "$LJ" && ! grep -q libvqe_tap "$LJ"; then
  echo "[STEP:fw:fail] чужой LD_PRELOAD в launch.json"
  die "в launch.json уже есть чужой LD_PRELOAD — автоматически не меняю, чтобы ничего не сломать"
fi
echo "[STEP:fw:ok] Прошивка совместима (VQE найден, aarch64)"

echo "[STEP:backup:start] Создание бэкапа launch.json"
ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
mount -o remount,rw / || die "не удалось remount rw"
restore_ro() { sync; [ "$WAS_RW" = 1 ] || mount -o remount,ro / 2>/dev/null; }
trap restore_ro EXIT

# Отправляем исходный конфиг для локального бэкапа на ПК
echo "LAUNCH_BACKUP_BEGIN"
base64 "$LJ" 2>/dev/null
echo "LAUNCH_BACKUP_END"

if [ -f "$BAK" ] && ! grep -q libvqe_tap "$BAK"; then
  [ -f "$BAK_DATA" ] || cp "$BAK" "$BAK_DATA" 2>/dev/null
  echo "бэкап launch.json уже сохранён"
elif [ -f "$BAK_DATA" ] && ! grep -q libvqe_tap "$BAK_DATA"; then
  cp "$BAK_DATA" "$BAK" 2>/dev/null
  echo "бэкап launch.json синхронизирован из $BAK_DATA"
elif ! grep -q libvqe_tap "$LJ"; then
  cp "$LJ" "$BAK" && cp "$LJ" "$BAK_DATA" 2>/dev/null || die "не удалось сделать бэкап launch.json"
  echo "бэкап launch.json успешно создан ($BAK)"
else
  sed '/libvqe_tap/d; /ALICE_TAP_TARGET/d' "$LJ" | sed '/CA_CERTIFICATES/s/,$//' > "$TMP"
  A=$(tr -cd '{[' < "$LJ" | wc -c); B=$(tr -cd '{[' < "$TMP" | wc -c)
  if [ "$A" = "$B" ] && ! grep -q libvqe_tap "$TMP"; then
    cp "$TMP" "$BAK" && cp "$TMP" "$BAK_DATA" 2>/dev/null
    echo "чистый бэкап launch.json сформирован и сохранён ($BAK)"
  fi
  rm -f "$TMP"
fi
echo "[STEP:backup:ok] Бэкап сохранён"

echo "[STEP:install:start] Установка libvqe_tap.so"
mkdir -p "$D" || die "не удалось создать $D"
SELF=$(dirname "$0")
if [ -f "$SELF/libvqe_tap.so" ]; then
  cp "$SELF/libvqe_tap.so" "$D/libvqe_tap.so.new" && mv "$D/libvqe_tap.so.new" "$D/libvqe_tap.so" || die "не удалось скопировать libvqe_tap.so"
elif [ -n "$EMBEDDED_SO_B64" ]; then
  echo "$EMBEDDED_SO_B64" | base64 -d > "$D/libvqe_tap.so.new" && mv "$D/libvqe_tap.so.new" "$D/libvqe_tap.so" || die "не удалось извлечь libvqe_tap.so из base64"
elif [ ! -f "$D/libvqe_tap.so" ]; then
  die "не найден libvqe_tap.so"
fi
chmod 755 "$D/libvqe_tap.so" 2>/dev/null
MD5_GOT=$(md5sum "$D/libvqe_tap.so" 2>/dev/null | cut -d' ' -f1)
echo "MD5 libvqe_tap.so: $MD5_GOT"
echo "[STEP:install:ok] libvqe_tap.so установлен"

echo "[STEP:target:start] Настройка launch.json"
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
  echo "launch.json успешно пропатчен"
fi
echo "[STEP:target:ok] Звук направлен на $PCIP:$PORT"

echo "[STEP:verify:start] Проверка результата"
grep -q "ALICE_TAP_TARGET=$PCIP:$PORT" "$LJ" || die "в launch.json не нашёл ALICE_TAP_TARGET=$PCIP:$PORT"
rm -f "$TMP"
echo "[STEP:verify:ok] launch.json проверен, права доступа настроены"

echo "Готово. Бэкап сохранён. Нужна перезагрузка колонки. Микрофон будет стримиться в udp://$PCIP:$PORT"
echo "RESULT=OK"
