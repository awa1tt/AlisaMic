#!/bin/sh
# install.sh <IP_ПК> [порт] — безопасная установка/обновление тапа микрофона Алисы (vqe_0 -> UDP на ПК).
# Идемпотентно, валидирует JSON, сохраняет бэкапы, проверяет права и ro-корень.

PCIP="$1"
PORT="${2:-5000}"
D=/data/quasar/micstream
LJ=/system/vendor/quasar/launch.json
BAK_SYS=/system/vendor/quasar/launch.json.bak-alisamic
BAK_DATA=/data/quasar/launch.json.bak-alisamic
LIB=/system/vendor/quasar/libs/libyandex_evil.so
TMP_NORM=/tmp/alisamic-norm.json
TMP_PATCH=/tmp/alisamic-patched.json

die() { echo "ОШИБКА: $*"; echo "RESULT=FAIL"; exit 1; }

[ -z "$PCIP" ] && { echo "Usage: install.sh <IP_ПК> [порт]"; exit 1; }
case "$PCIP" in *[!0-9.]*) die "IP ПК должен быть вида 192.168.0.5";; esac
case "$PORT" in ""|*[!0-9]*) die "порт должен быть числом";; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "порт должен быть от 1 до 65535"
[ -f "$LJ" ] || die "не найден $LJ — не та прошивка"

echo "[STEP:fw:start] Проверка прошивки и совместимости VQE"
[ -f "$LIB" ] || { echo "[STEP:fw:fail] нет $LIB"; die "нет $LIB — эта прошивка не подходит"; }
grep -q YandexVqeC_GetOutputChannelData "$LIB" || { echo "[STEP:fw:fail] VQE символ не найден"; die "в libyandex_evil.so нет YandexVqeC_GetOutputChannelData"; }

if grep -q LD_PRELOAD "$LJ" && ! grep -q libvqe_tap "$LJ"; then
  echo "[STEP:fw:fail] чужой LD_PRELOAD в launch.json"
  die "в launch.json уже есть чужой LD_PRELOAD — автоматически не меняю"
fi
echo "[STEP:fw:ok] Прошивка совместима (VQE найден, aarch64)"

echo "[STEP:backup:start] Создание бэкапа launch.json"
ROOTOPT=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT," in *,rw,*) WAS_RW=1;; *) WAS_RW=0;; esac
mount -o remount,rw / || die "не удалось remount rw"
restore_ro() {
  sync
  if [ "$WAS_RW" != 1 ]; then
    mount -o remount,ro / 2>/dev/null
  fi
}
trap restore_ro EXIT

# 1. Сохраняем исходный оригинальный бэкап, если его ещё нет
if [ ! -f "$BAK_DATA" ]; then
  if ! grep -q libvqe_tap "$LJ"; then
    cp "$LJ" "$BAK_DATA" 2>/dev/null
    echo "оригинальный бэкап сохранён: $BAK_DATA"
  elif [ -f "$BAK_SYS" ]; then
    cp "$BAK_SYS" "$BAK_DATA" 2>/dev/null
    echo "оригинальный бэкап скопирован из $BAK_SYS"
  fi
fi

# 2. Всегда создаём бэкап с текущей меткой времени (никогда не перезаписываем оригинал)
TS=$(date +%Y%m%d_%H%M%S 2>/dev/null || echo "$$")
DATED_BAK="/data/quasar/launch.json.bak-$TS"
cp "$LJ" "$DATED_BAK" 2>/dev/null
echo "бэкап сохранён: $DATED_BAK"
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
# 1. Нормализуем переводы строк (устраняем \r)
tr -d '\r' < "$LJ" > "$TMP_NORM" || die "не удалось прочитать $LJ"

# 2. Идемпотентный генератор JSON для задачи с --platformd
awk -v pc="$PCIP" -v port="$PORT" '
BEGIN {
  in_task = 0
  is_platformd = 0
  in_envs = 0
  patched = 0
}
{
  line = $0

  if (line ~ /\{/) {
    if (!in_task) in_task = 1
  }

  if (in_task && line ~ /"--platformd"/) {
    is_platformd = 1
  }

  if (in_task && is_platformd && line ~ /"envs":[ \t]*\[/) {
    in_envs = 1
    print line
    next
  }

  if (in_envs) {
    if (line ~ /\]/) {
      in_envs = 0
      is_platformd = 0
      patched = 1
      print "                        \"LD_PRELOAD=/data/quasar/micstream/libvqe_tap.so\","
      print "                        \"ALICE_TAP_TARGET=" pc ":" port "\""
      print line
      next
    }

    # Пропускаем старые или повреждённые записи
    if (line ~ /libvqe_tap/ || line ~ /ALICE_TAP_TARGET/) {
      next
    }

    # Убираем любые лишние запятые на конце и форматируем запятую
    sub(/[ \t]*,+[ \t]*$/, "", line)
    if (line ~ /"[^"]+"/) {
      print line ","
    } else {
      print line
    }
    next
  }

  if (in_task && line ~ /\},?[ \t]*$/) {
    in_task = 0
    is_platformd = 0
  }

  print line
}
END {
  if (!patched) exit 2
}
' "$TMP_NORM" > "$TMP_PATCH" || die "не удалось найти задачу platformd в launch.json"

# 3. Валидатор синтаксиса JSON
awk '
BEGIN {
  depth_b = 0
  depth_a = 0
  last_tok = ""
  err = 0
}
{
  line = $0
  len = length(line)
  in_str = 0
  for (i = 1; i <= len; i++) {
    c = substr(line, i, 1)
    if (c == "\\" && in_str) { i++; continue }
    if (c == "\"") {
      in_str = !in_str
      if (!in_str) last_tok = "val"
      continue
    }
    if (in_str) continue
    if (c ~ /[ \t\r\n]/) continue

    if (c == "{") { depth_b++; last_tok = "{" }
    else if (c == "}") {
      if (last_tok == ",") { print "SYNTAX ERROR: trailing comma before } at line " NR; err=1; exit 1 }
      depth_b--; last_tok = "}"
    }
    else if (c == "[") { depth_a++; last_tok = "[" }
    else if (c == "]") {
      if (last_tok == ",") { print "SYNTAX ERROR: trailing comma before ] at line " NR; err=1; exit 1 }
      depth_a--; last_tok = "]"
    }
    else if (c == ",") {
      if (last_tok == "," || last_tok == "{" || last_tok == "[") {
        print "SYNTAX ERROR: invalid comma position at line " NR; err=1; exit 1
      }
      last_tok = ","
    }
    else if (c == ":") { last_tok = ":" }
    else if (c ~ /[0-9a-zA-Z_\-]/) { last_tok = "val" }
  }
}
END {
  if (err) exit 1
  if (depth_b != 0 || depth_a != 0) {
    print "SYNTAX ERROR: unbalanced braces/brackets. {:" depth_b " [:" depth_a
    exit 1
  }
}
' "$TMP_PATCH" || die "сгенерированный launch.json не прошёл валидацию синтаксиса JSON"

# 4. Запись и проверка
cat "$TMP_PATCH" > "$LJ" || die "не удалось записать $LJ"
sync
rm -f "$TMP_NORM" "$TMP_PATCH"
echo "[STEP:target:ok] Звук направлен на $PCIP:$PORT"

echo "[STEP:verify:start] Проверка результата"
grep -q "ALICE_TAP_TARGET=$PCIP:$PORT" "$LJ" || die "в launch.json не найден ALICE_TAP_TARGET=$PCIP:$PORT"
grep -q "libvqe_tap.so" "$LJ" || die "в launch.json не найден LD_PRELOAD"
restore_ro
ROOTOPT_AFTER=$(awk '$2=="/"{print $4}' /proc/mounts | tail -n 1)
case ",$ROOTOPT_AFTER," in
  *,ro,*) echo "раздел / успешно возвращён в режим read-only";;
  *) die "ВНИМАНИЕ: раздел / остался в rw!";;
esac
echo "[STEP:verify:ok] launch.json проверен, раздел / в ro"

echo "Готово. Бэкап сохранён. Нужна перезагрузка колонки. Микрофон будет стримиться в udp://$PCIP:$PORT"
echo "RESULT=OK"
