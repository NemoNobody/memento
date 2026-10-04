# memento

[![CI](https://github.com/NemoNobody/memento/actions/workflows/ci.yml/badge.svg)](https://github.com/NemoNobody/memento/actions/workflows/ci.yml)

Персистентная память с provenance — для AI-агентов (и людей).

[English version](README.md)

## Что это

`memento` хранит **проверенные факты** о ваших проектах — расположение файлов,
значения endpoint'ов и конфигов, поведение — в индексе OpenSearch, чтобы одно и
то же не приходилось перепроверять снова и снова — ни вам, ни агенту.

Каждый факт несёт улики, а не вердикты:

- **provenance** (`source`): откуда взято — `tool:read_file`, `experiment`,
  `user:confirmed`, `doc:*`;
- **время наблюдения** (`observed_at`): когда значение реально видели;
- **историю**: при смене значения старое уходит в `history`, а не затирается;
- **пометки** (`notes`): оговорки, например «проверил только dev-конфиг».

Статуса два: `verified` (субъект существует) и `obsolete` (субъект исчез —
файл удалён, endpoint снят). Store хранит улики; агент сам судит о доверии и
устаревании в контексте.

## Быстрый старт

Сборка (Go 1.25.9+):

```
go build -o memento .
```

Или установка напрямую (когда модуль станет публичным и получит тег):

```
go install github.com/NemoNobody/memento@latest
```

Настройка: скопируйте `memento.conf.example` в `memento.conf` (или
`~/.memento.conf`) и заполните подключение к OpenSearch.

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "opensearch_password": "<ваш-пароль>",
  "insecure_skip_verify": true,
  "index_name": "memento"
}
```

Индекс создаётся автоматически при первом обращении. Env-переменные
перекрывают файл: `OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_INDEX`; `MEMENTO_CONF` задаёт
явный путь к конфигу. Проверить связь:

```
memento status
```

Сохранить и прочитать факт:

```
memento add demo:api:port "API port is 8080" endpoint -p demo -s tool:read_file
memento get demo:api:port
memento search "api port"
```

## Команды

| Команда | Назначение |
|---------|------------|
| `memento add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>]` | Добавить или скорректировать факт (старое значение уходит в историю) |
| `memento get <key>` | Получить факт по ключу |
| `memento search <query> [--project <p>] [--category <c>] [--status <s>]` | Полнотекстовый поиск по значениям и заметкам |
| `memento list <project>` | Все факты проекта |
| `memento obsolete <key> [reason]` | Пометить факт obsolete: субъект исчез (причина дописывается в notes) |
| `memento semantic <query> [--top-k 5]` | Семантический поиск (нужна embedding-модель, пока не подключён) |
| `memento status` | Проверить связь и показать действующую конфигурацию |
| `memento mcp` | Запустить как MCP-сервер поверх stdio |

Конвенция ключа: ключ называет **устойчивый субъект**, а не текущее значение —
`demo:api:port`, не `demo:api:v1`. Повторное использование того же ключа с новым
значением — это коррекция, старое значение остаётся в истории; повтор с тем же
значением обновляет только заметки.

## MCP-сервер

`memento mcp` говорит на MCP поверх stdio, поэтому агенты (VS Code и другие)
вызывают `memento_status`, `memento_search`, `memento_get`, `memento_add`,
`memento_list` и `memento_obsolete` напрямую. Регистрация и диагностика:
[docs/mcp.md](docs/mcp.md).

## Для агентов

Готовое правило — [rules/memento.md](rules/memento.md): скопируйте его (или
только компактный блок) в `AGENTS.md`, `.github/copilot-instructions.md`,
`.cursor/rules/` или always-on инструкции вашего редактора. Оно объясняет агенту,
когда заглядывать в память, когда её обновлять — и как не соврать.

## Хранилище

Факты лежат в одном индексе OpenSearch (по умолчанию `memento`). Для старта
достаточно одной ноды; полноценный кластер работает так же. Как направить
`memento` на кластер: [docs/setup.md](docs/setup.md). Схема и устройство:
[docs/how-it-works.md](docs/how-it-works.md).

**Никогда не храните секреты** (пароли, токены, приватные ключи): индекс —
plaintext.

## Лицензия

[GPL-3.0](LICENSE)
