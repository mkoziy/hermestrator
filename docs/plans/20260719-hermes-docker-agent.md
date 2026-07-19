# Plan: Hermes coding-agent Docker image

Собрать Docker-образ кодинг-агента на базе Hermes Agent (nousresearch/hermes-agent), с git/gh/fzf/jq/ripgrep, Go-тулчейном, Node.js, codex CLI, pi CLI и ralphex-бинарником внутри; скопировать в образ ralphex-профили (codex/pi/claude) из `ralphex/`; настроить идемпотентный entrypoint и self-backup в приватный GitHub-репозиторий через встроенный cron Hermes.

k3s-деплой (StatefulSet/Deployment, manifests, Secret) вынесен из этой итерации — пока только образ и локальная сборка/проверка. Вернуться к деплою отдельным планом позже.

Все архитектурные решения (base image, non-root user, entrypoint idempotency rules, backup flow, env-var-only secrets) уже приняты в брейнсторм-сессии — не переоткрывать их. Версии (Go, Node LTS, ralphex release) проверять заново на момент выполнения задачи, не брать как константы из этого файла.

**Предположения (TODO, при необходимости скорректировать перед/во время выполнения):**
- Имя образа: `ghcr.io/mkoziy/hermes-coding-agent` (registry — GHCR, т.к. `gh`/GitHub уже в центре стека). Скорректировать, если нужен другой registry.
- Расписание cron-бэкапа: ежедневно в 03:00 UTC, плюс возможность ручного триггера командой пользователю в чате. Скорректировать по факту.

**Подтверждено сверкой с реальным репозиторием/докой Hermes (`github.com/NousResearch/hermes-agent`, `hermes-agent.nousresearch.com/docs`) на 2026-07-19 — не перепроверять существование этих команд/страниц заново, но версии/синтаксис флагов сверять по месту:**
- `hermes config set|get`, `hermes doctor`, `hermes cron add|edit|runs`, `hermes gateway install|setup|start|status|stop` — реальные подкоманды, подтверждены в README и `docs/reference/cli-commands`.
- У Hermes два независимых входа: `hermes` — интерактивный TUI; `hermes gateway` — headless-процесс для messaging-платформ (Telegram/Discord/Slack/WhatsApp/Signal/Email) + встроенный cron. Для этого деплоя (доступ только через messaging/gateway) PID 1 контейнера почти наверняка должен быть `hermes gateway`, не голый `hermes` — see Task 6.
- Есть официальная страница `docs/user-guide/security` с разделами "Dangerous Command Approval" (approval modes, YOLO mode), "DM Pairing System", "Container Isolation", "Terminal Backend Security Comparison", "Environment Variable Passthrough" — это готовый гайд именно под этот сценарий (Hermes в контейнере, headless), прочитать целиком перед Task 6, не изобретать заново.
- Hermes сам умеет запускать команды через отдельный "Docker" terminal backend (один из шести: local/Docker/SSH/Singularity/Modal/Daytona) — это Hermes, спавнящий контейнеры для sandboxing инструментов, ОТДЕЛЬНО от того, что сам Hermes работает в нашем контейнере. Если backend по умолчанию — `docker`, а `docker.sock`/DinD в контейнере нет, тулинг сломается на первом вызове инструмента. Явно выставить terminal backend в `local`.
- Установщик управляет собственным Node.js/Python 3.11 через `uv`, если системные версии отсутствуют/устарели (переписывает npm global prefix под себя). Дублирующая установка Node/Python в Task 3 — реальный риск коллизии PATH/npm-prefix, не только гипотетический.

## Validation Commands
- `docker build -t hermes-coding-agent:local .`
- `docker run --rm hermes-coding-agent:local hermes doctor` (ожидаем осмысленный вывод диагностики, не краш)
- `docker run --rm hermes-coding-agent:local bash -lc 'go version && node --version && python3 --version && ralphex --version && codex --version && pi --version && gh --version && git --version && fzf --version && jq --version'`
- `hadolint Dockerfile` (если доступен локально; иначе пропустить и отметить в progress)

### Task 1: Инициализировать git-репозиторий и базовую структуру
- [x] Репозиторий `/Users/michael/github.com/mkoziy/coding` сейчас не git-репозиторий — выполнить `git init`, создать `.gitignore` (как минимум: `.env`, `*.env`, `**/*secret*`, `**/credentials*`, `progress/`, `worktrees/` — последние два уже есть в `ralphex/ralphex-pi/.gitignore`, унифицировать на верхнем уровне)
- [x] Создать `docker/` каталог для Docker-артефактов (или разместить в корне — решить по месту, главное консистентно с остальным репо)
- [x] Сделать первый коммит с текущим состоянием (`ralphex/` конфиги как есть)

### Task 2: Собрать Dockerfile — базовый слой и системные пакеты
- [x] `FROM debian:bookworm-slim`, `ARG GO_VERSION`, `ARG NODE_MAJOR` как build args с дефолтами (значения подставить актуальные на момент сборки — проверить `curl -s https://go.dev/VERSION?m=text` для Go и текущий Node LTS major)
- [x] Создать непривилегированного пользователя `app` (`useradd -m -s /bin/bash app`), `HOME=/home/app`
- [x] Установить system packages: `git curl ca-certificates jq ripgrep build-essential ffmpeg unzip fzf` через `apt-get install --no-install-recommends`, почистить apt-кэш в том же слое
- [x] Установить `gh` CLI через официальный apt-репозиторий (`cli.github.com`)

### Task 3: Установить Go, Node.js, Python/uv тулчейны
- [x] Скачать и распаковать Go tarball (`https://go.dev/dl/go${GO_VERSION}.linux-<arch>.tar.gz`) в `/usr/local/go`, добавить `/usr/local/go/bin` в `PATH`, задать `GOPATH=/home/app/go` для пользователя `app`
- [x] Установить Node.js LTS через NodeSource setup-скрипт
- [x] Установить `uv` (astral) явно (`curl -LsSf https://astral.sh/uv/install.sh | sh`), Python 3.11 через uv или system python3.11 — сверено с реальным install.sh: Hermes ВСЕГДА ставит собственный Python 3.11 через `uv python install`/`uv venv`, независимо от системного python3 — конфликта с system python3 нет (в отличие от Node). System `python3`/`python3-venv` установлены через apt для общей доступности и валидационных команд плана
- [x] Проверить, что все три рантайма доступны в `PATH` для пользователя `app` (не только root) — `PATH`/`GOPATH` заданы через `ENV` (действуют для всех пользователей образа), `/home/app/go` создан и `chown`-нут на `app`
- [x] Подтверждённый риск (не гипотетический): Hermes installer сам ставит управляемый Node.js в `$HERMES_HOME/node` и переписывает npm global prefix (`$HERMES_HOME/node/etc/npmrc`), если найденный системный Node не проходит `node_satisfies_build` (`^20.19 || >=22.12`). Решено: системный Node ставится версией NODE_MAJOR=24 (>=22.12) через NodeSource — сверено напрямую по коду install.sh (`node_satisfies_build`), installer обнаружит этот Node как достаточный и не будет трогать Node вообще

### Task 4: Установить Hermes Agent, Mnemosyne, codex, pi, ralphex
- [ ] Переключиться на `USER app`, `WORKDIR /home/app`
- [ ] Установить Hermes: `curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash` — НЕ вызывать `hermes setup` на этом шаге (интерактивный, требует ключи)
- [ ] `pip install "mnemosyne-memory[all]"` (или через uv) — сверить, не конфликтует ли с версией, которую тянет сам Hermes installer
- [ ] `npm install -g` для codex CLI и pi CLI — сверить актуальные имена npm-пакетов на момент установки (могли измениться с момента брейнсторма)
- [ ] Скачать последний релиз `umputun/ralphex` под нужную архитектуру с GitHub Releases API, положить бинарник в `/usr/local/bin/ralphex`, `chmod +x`
- [ ] Проверить версии всех пяти инструментов в самом образе (см. Validation Commands)

### Task 5: Скопировать ralphex-профили и написать переключатель профилей
- [ ] `COPY --chown=app:app ralphex/ralphex-codex/config /opt/ralphex-profiles/codex/config`
- [ ] `COPY --chown=app:app ralphex/ralphex-pi/{config,agents,prompts,scripts} /opt/ralphex-profiles/pi/` (сохранить внутреннюю структуру подпапок)
- [ ] `COPY --chown=app:app ralphex/ralphex-claude/config /opt/ralphex-profiles/claude/config`
- [ ] `chmod +x` на все `.sh`-скрипты внутри скопированных `scripts/` (`pi-as-claude.sh`, `pi-opencode-go.sh` и любые другие)
- [ ] Написать `/usr/local/bin/ralphex-use-profile.sh`: принимает один аргумент (`codex|pi|claude`), валидирует его, делает `rm -rf ~/.config/ralphex && cp -r /opt/ralphex-profiles/$1/* ~/.config/ralphex/` (или symlink — решить, что проще поддерживать; symlink избегает копирования, но COPY-слой всё равно read-only для образа, так что копирование в volume проще при первом старте)

### Task 6: Написать entrypoint.sh (идемпотентный)
- [ ] **Перед написанием entrypoint — прочитать целиком `docs/user-guide/security` (Dangerous Command Approval / YOLO Mode / DM Pairing System / Container Isolation / Terminal Backend Security Comparison / Environment Variable Passthrough) и `docs/user-guide/messaging`** на `hermes-agent.nousresearch.com/docs` — эти страницы прямо описывают целевой сценарий (headless Hermes в контейнере), не изобретать решения ниже вслепую
- [ ] Проверка первого запуска: `[ -d "$HERMES_HOME" ] && [ -n "$(ls -A "$HERMES_HOME" 2>/dev/null)" ]` — если volume пуст И задан `HERMES_BACKUP_REPO`, клонировать бэкап-репозиторий в `$HERMES_HOME` (восстановление состояния); если пуст и репозиторий не задан/не существует — обычная первая инициализация
- [ ] Настроить git identity из env (`GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL` или `GIT_USER_NAME`/`GIT_USER_EMAIL` — выбрать единый набор имён переменных и задокументировать в README)
- [ ] `gh auth login` через `GH_TOKEN` (неинтерактивно, `gh auth login --with-token <<< "$GH_TOKEN"` или `echo $GH_TOKEN | gh auth login --with-token`)
- [ ] Неинтерактивная конфигурация провайдера Hermes из env: для каждого нужного `hermes config set <key> <value>` — сначала `hermes config get <key>`, ставить только если отличается (idempotent write). Команда подтверждена реальной (см. шапку плана)
- [ ] Явно решить и задать режим одобрения опасных команд для headless-запуска (без TTY команды подтверждать некому) — либо `HERMES_YOLO_MODE=1` (полный автопропуск), либо настроить DM Pairing System так, чтобы одобрения приходили пользователю в чат мессенджера; задокументировать выбор и его риски в README (Task 9), не оставлять как implicit default
- [ ] Явно выставить terminal backend в `local` (env var/`hermes config set`, сверить точное имя ключа по `docs/user-guide/security#terminal-backend-security-comparison`) — без этого возможен неявный fallback на `docker`-backend, которому нужен `docker.sock`/DinD, не предусмотренный в этом поде
- [ ] Рассмотреть `HERMES_GATEWAY_NO_SUPERVISE` (или актуальный эквивалент): у `hermes gateway` есть собственный internal restart-loop (`HERMES_GATEWAY_MAX_STARTS`), который может конфликтовать с внешним supervisor'ом контейнера (`docker restart`, будущий k8s restart policy) — решить, кто супервайзит рестарты, не оставлять оба уровня активными без проверки
- [ ] Вызвать `ralphex-use-profile.sh "${RALPHEX_DEFAULT_PROFILE:-claude}"`
- [ ] НЕ трогать/не удалять `*.db-wal` / `*.db-shm` файлы mnemosyne при старте
- [ ] Зарегистрировать/убедиться, что cron-задача бэкапа существует в Hermes (см. Task 7) — идемпотентно, через `hermes cron` (list/get подкоманды подтверждены реальными, см. шапку плана)
- [ ] Финально: **`exec hermes gateway` (не голый `hermes`)** — `hermes` без аргументов запускает интерактивный TUI, а доступ к этому деплою предполагается только через messaging-платформы/gateway (см. шапку плана); foreground, PID 1, без supervisord — если по факту окажется не так (например, нужен `hermes` + отдельно поднятый gateway), задокументировать почему и скорректировать

### Task 7: Написать hermes-backup.sh и настроить cron внутри Hermes
- [ ] Скрипт `hermes-backup.sh`: `cd "$HERMES_HOME" && git add -A && git diff --cached --quiet || git commit -m "backup $(date -u +%FT%TZ)"`, затем `git push origin <branch>` (без `--force`)
- [ ] Создать/проверить `.gitignore` внутри `$HERMES_HOME` (генерируется при первой инициализации, если отсутствует): исключить `**/*secret*`, `**/credentials*`, `.env`, любые файлы с токенами, которые Hermes мог бы туда положить (сверить актуальный список чувствительных файлов в `$HERMES_HOME` по документации Hermes на момент выполнения — могло измениться)
- [ ] Настроить cron-задачу через `hermes cron add` (подкоманда подтверждена реальной — есть `add`/`edit`/`runs`, см. шапку плана; точный синтаксис флагов сверить в `docs/user-guide/features/cron`) на ежедневный запуск `hermes-backup.sh` (расписание — TODO из шапки плана, подтвердить с пользователем при выполнении)
- [ ] Задокументировать, как вручную запустить бэкап (на случай, если пользователь попросит "забэкапься сейчас")

### Task 8: .dockerignore и верхнеуровневый README
- [ ] `.dockerignore`: `.git`, `docs/plans/completed`, локальные `.env`
- [ ] `README.md`: как собрать образ локально, как пушить в GHCR, как запустить контейнер локально (`docker run` с `.env`), список обязательных env-переменных с описанием (включая `HERMES_YOLO_MODE`/approval-режим, terminal backend, `HERMES_GATEWAY_NO_SUPERVISE` из Task 6), как переключить ralphex-профиль вручную, как посмотреть логи бэкапа, известные ограничения. Явно отметить, что деплой в k3s — предмет отдельного плана, не этого

### Task 9: Локальная сборка и сквозная проверка
- [ ] `docker build` образа, прогнать все Validation Commands из шапки плана
- [ ] Запустить контейнер локально с тестовым `.env` (фиктивные/тестовые ключи или реальные dev-ключи по решению пользователя), проверить: `hermes doctor` зелёный, `ralphex-use-profile.sh pi && cat ~/.config/ralphex/config` показывает pi-профиль, `hermes-backup.sh` создаёт коммит в тестовом репозитории
- [ ] Замерить время холодного старта (пригодится для калибровки проб здоровья, когда дойдём до деплоя)
- [ ] Прогнать restart-сценарий: убить контейнер (`docker kill`), поднять заново с тем же volume, убедиться что entrypoint не падает и не дублирует инициализацию (проверка идемпотентности вручную)
