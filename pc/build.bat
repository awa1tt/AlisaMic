@echo off
rem Сборка AlisaMic.exe (нужен Go 1.23+: https://go.dev/dl/)
rem Иконка и манифест уже лежат в rsrc_windows_*.syso.
rem Если поменяли winres\icon.png:  go install github.com/tc-hib/go-winres@v0.3.3 ^&^& go-winres make --arch amd64,386,arm64
cd /d "%~dp0"
rem файлы для колонки вшиваются в exe из pc\embed — обновляем их из station\
copy /y "..\station\libvqe_tap.so" "embed\libvqe_tap.so" >nul
copy /y "..\station\install.sh" "embed\install.sh" >nul
go build -trimpath -ldflags "-s -w" -o ..\AlisaMic.exe . || (pause & exit /b 1)
echo Готово: AlisaMic.exe
