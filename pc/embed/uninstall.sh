#!/bin/sh
# Откат установки AlisaMic: полностью возвращает launch.json из бэкапа и удаляет libvqe_tap.so
D=/data/quasar/micstream
LJ=/system/vendor/quasar/launch.json
BAK=/system/vendor/quasar/launch.json.bak-alisamic
BAK_DATA=/data/quasar/launch.json.bak-alisamic
TMP=/tmp/alisamic-launch-restore.new

die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }

ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
mount -o remount,rw / || die "не удалось remount rw"
restore_ro() { sync; [ "$WAS_RW" = 1 ] || mount -o remount,ro / 2>/dev/null; }
trap restore_ro EXIT

RESTORED=0

# 1. Возвращаем launch.json из чистого бэкапа
if [ -f "$BAK" ] && ! grep -q libvqe_tap "$BAK"; then
  cat "$BAK" > "$LJ" || die "не удалось записать launch.json из бэкапа $BAK"
  echo "конфигурация launch.json успешно возвращена из бэкапа ($BAK)"
  RESTORED=1
elif [ -f "$BAK_DATA" ] && ! grep -q libvqe_tap "$BAK_DATA"; then
  cat "$BAK_DATA" > "$LJ" || die "не удалось записать launch.json из бэкапа $BAK_DATA"
  echo "конфигурация launch.json успешно возвращена из бэкапа ($BAK_DATA)"
  RESTORED=1
fi

# 2. Если файла бэкапа не было, но launch.json пропатчен — очищаем от строк патча
if [ "$RESTORED" = "0" ] && grep -q libvqe_tap "$LJ" 2>/dev/null; then
  echo "файл бэкапа не найден, удаляю строки патча напрямую из launch.json..."
  sed '/libvqe_tap/d; /ALICE_TAP_TARGET/d' "$LJ" | sed '/CA_CERTIFICATES/s/,$//' > "$TMP"
  A=$(tr -cd '{[' < "$LJ" | wc -c); B=$(tr -cd '{[' < "$TMP" | wc -c)
  [ "$A" = "$B" ] || die "ошибка при очистке launch.json: баланс скобок нарушен"
  cat "$TMP" > "$LJ" || die "не удалось обновить launch.json"
  rm -f "$TMP"
  echo "launch.json очищен от изменений AlisaMic и возвращён в исходный вид"
  RESTORED=1
fi

if [ "$RESTORED" = "0" ]; then
  echo "launch.json уже в исходном виде (патч не обнаружен)"
fi

# 3. Удаляем файлы AlisaMic и бэкапы
rm -f "$D/libvqe_tap.so" "$D/libvqe_tap.so.new"
rm -f "$BAK" "$BAK_DATA"
rmdir "$D" 2>/dev/null
echo "все файлы AlisaMic на колонке удалены"
echo "Нужна перезагрузка колонки."
echo "RESULT=OK"
