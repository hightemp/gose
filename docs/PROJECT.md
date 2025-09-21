# Поисковая система: архитектура и запуск

Проект состоит из нескольких сервисов:
- Краулер: [search_crawler_service](search_crawler_service/)
- Веб‑интерфейс поиска: [search_ui_service](search_ui_service/)
- Генератор доменов/добавление в очередь индексации: [domain_search_service](domain_search_service/)
- Менеджер сайтов (каркас): [site_manager_service](site_manager_service/)
- Сервис прокси (внешний, уже существует в репозитории): [proxy_checker_service](proxy_checker_service/)

Хранилище: PostgreSQL 16 с FTS (russian/en + unaccent), хранение и исходного HTML, и извлеченного текста.

Основные артефакты развёртывания:
- Compose: [deploy/docker-compose.yml](deploy/docker-compose.yml)
- Инициализация БД: [deploy/db/init.sql](deploy/db/init.sql)
- Конфиги: 
  - Краулер: [deploy/crawler.config.yaml](deploy/crawler.config.yaml)
  - Поисковый UI: [deploy/search_ui.config.yaml](deploy/search_ui.config.yaml)
  - Генератор доменов: [deploy/domain_search.config.yaml](deploy/domain_search.config.yaml)
  - Прокси: [deploy/proxies.yaml](deploy/proxies.yaml)
- Переменные окружения (compose): [.env](.env)

## Архитектура и потоки данных

- domain_search_service генерирует доменные имена, проверяет их «рабочесть» HTTP‑запросом (ограничение тела; 200..399 счёт успешным), для успешных:
  1) гарантирует наличие записи сайта в таблице sites
  2) добавляет корневой URL (https://domain/) в очередь crawl_queue со статусом queued
- search_crawler_service выбирает задачи из очереди в базе, соблюдая ограничения (rate‑limit per host), загружает HTML через пул прокси, извлекает текст/метаданные, сохраняет в pages (html + text), формирует tsvector_ru/tsvector_en
- search_ui_service работает поверх PostgreSQL FTS: websearch_to_tsquery(ru|en), ранжирование ts_rank_cd, подсветка ts_headline, пагинация

Схема БД задаётся в [deploy/db/init.sql](deploy/db/init.sql):
- sites, site_seeds, crawl_queue, pages (html + text + FTS), page_links, robots_cache, sitemaps
- FTS: tsvector_ru, tsvector_en, индексы GIN, функция/триггер обновления tsvector на основе text; расширения unaccent, pg_trgm

## Конфигурация

- Подключение к базе настроено в .env для docker compose и сервисов:
  - [.env](.env)
  - Внутри docker‑сети DSN: postgres://search:search@postgres:5432/search?sslmode=disable
- Сервисные YAML:
  - Краулер: [deploy/crawler.config.yaml](deploy/crawler.config.yaml)
    - домены whitelist, seed‑urls (для ручной загрузки), лимиты RPS, размеры, типы контента, языки
    - путь к списку прокси [deploy/proxies.yaml](deploy/proxies.yaml)
  - Поиск (UI): [deploy/search_ui.config.yaml](deploy/search_ui.config.yaml)
    - адрес HTTP сервера, параметры сниппетов/подсветки, каталог шаблонов
  - Генератор доменов: [deploy/domain_search.config.yaml](deploy/domain_search.config.yaml)
    - профиль генерации (TLD, длина, алфавит и ограничения «-»)
    - лимиты: concurrency, global RPS, предел генерации
    - HTTP‑проверка «рабочести» (метод/таймаут/ретраи/ограничение тела/диапазон кодов/https‑сначала)

## Сервисы

- Краулер
  - Код: [search_crawler_service/main.go](search_crawler_service/main.go)
  - HTTP:
    - GET /healthz — состояние, параметры, проверка ping к БД
    - POST /api/enqueue — (MVP API) добавить URL в очередь (служебный интерфейс, для совместимости; domain_search пишет напрямую в БД)
  - Очередь: crawl_queue с partial unique по (site_id,url_hash) для статусов queued/processing
  - Хранение: pages.html (исходный HTML) + pages.text (извлеченный текст) + tsv_ru/tsv_en
- Поисковый UI
  - Код: [search_ui_service/main.go](search_ui_service/main.go)
  - Шаблоны: [search_ui_service/templates/index.html](search_ui_service/templates/index.html), [search_ui_service/templates/results.html](search_ui_service/templates/results.html)
  - Переменная окружения: PG_DSN (из .env/compose)
- Генератор доменов
  - Код: [domain_search_service/main.go](domain_search_service/main.go)
  - Работает по профилю 3 (расширенный): TLD [.com, .net, .org, .ru], длина 2–15, алфавит [a‑z,0‑9,'-'] с ограничениями, проверка HTTP GET / (ограничение тела 32KB), 1 ретрай, 3s timeout, 200..399 — успешно
  - Пишет напрямую в БД (sites + crawl_queue), не через API

## Запуск (docker compose)

1) Установите Docker и Docker Compose
2) Проверьте/при необходимости правьте порты/DSN в файле [.env](.env)
3) Запустите:
   - Пример (из корня проекта):
     - PG_PORT=5433 CRAWLER_PORT=18082 SEARCH_UI_PORT=18080 docker compose -f [deploy/docker-compose.yml](deploy/docker-compose.yml) up -d --build
4) Проверка:
   - Краулер: curl http://localhost:18082/healthz
   - Поиск UI: открыть http://localhost:18080/
   - PostgreSQL: доступен на 5433 (локально)

Примечание: compose монтирует локальный каталог deploy/ внутрь контейнеров как /app/deploy (ro), поэтому изменения YAML в каталоге deploy/ подхватываются при рестарте контейнеров.

## Как добавить URL в очередь

Варианты:
- Через domain_search_service — сервис сам найдёт «рабочие» домены и положит https://domain/ в crawl_queue
- Через API краулера (вспомогательный путь, для интеграций):
  - POST /api/enqueue c JSON { "url": "https://example.com/", "priority": 0 }
  - Код обработчика см. [search_crawler_service/main.go](search_crawler_service/main.go)

## Поисковые запросы (пример)

- UI отправляет GET /search?q=запрос&page=1
- На стороне БД: OR‑запрос между websearch_to_tsquery('russian', $q) и websearch_to_tsquery('english', $q), ранжирование ts_rank_cd, подсветка ts_headline для обоих языков

## Прокси

- Формат: [deploy/proxies.yaml](deploy/proxies.yaml)
  - rotation: round_robin
  - ban_policy: consecutive_errors + ban_duration
  - healthcheck: метод/URL/таймаут/интервал
  - proxies: список URL (http, https, socks5; с поддержкой user:pass@)

## Развитие/план

Ближайшие задачи (MVP):
- Реализовать сам обход (worker) в краулере: SELECT FOR UPDATE SKIP LOCKED из crawl_queue, загрузка HTML с ротацией прокси, robots/sitemap/нормализация URL, сохранение pages + извлечение ссылок → пополнение crawl_queue
- Добавить HTMX в Search UI для интерактивной пагинации/фильтров
- Добавить Manager UI (аутентификация, CRUD sites/seed, действия recrawl, дашборд)

## Команды

- Локальная сборка сервисов (Go):
  - Краулер: [search_crawler_service/Makefile](search_crawler_service/Makefile)
  - Поиск UI: [search_ui_service/Makefile](search_ui_service/Makefile)
- Dockerfile:
  - Краулер: [search_crawler_service/Dockerfile](search_crawler_service/Dockerfile)
  - Поиск UI: [search_ui_service/Dockerfile](search_ui_service/Dockerfile)
  - Поиск доменов: [domain_search_service/Dockerfile](domain_search_service/Dockerfile)

## Примечания по производительности и стойкости

- Лимиты RPS/пер‑хост реализуются на стороне краулера (в планируемом worker‑пуле)
- Очередь на Postgres с SKIP LOCKED — подход без внешнего брокера (упростит MVP), но при росте нагрузки можно перейти на MQ (NATS/Rabbit) с сохранением совместимости
- Индексация FTS в триггере перед INSERT/UPDATE обеспечивает консистентность поиска
- Хранение исходного HTML позволяет визуализировать страницу в UI (в перспективе отдельный просмотрщик)
