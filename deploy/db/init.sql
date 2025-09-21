-- Database initialization for Search System (Crawler + Search UI + Manager UI)
-- Stores both original HTML (for viewing) and extracted text (for search)

-- Extensions
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Types
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'crawl_status') THEN
    CREATE TYPE crawl_status AS ENUM ('queued','processing','done','error');
  END IF;
END
$$;

-- Utility: updated_at trigger
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END
$$ LANGUAGE plpgsql;

-- FTS updater for pages.text -> tsv_ru/tsv_en
CREATE OR REPLACE FUNCTION pages_set_tsvectors() RETURNS trigger AS $$
BEGIN
  IF NEW.text IS NULL OR length(NEW.text) = 0 THEN
    NEW.tsv_ru := NULL;
    NEW.tsv_en := NULL;
  ELSE
    NEW.tsv_ru := to_tsvector('russian', unaccent(NEW.text));
    NEW.tsv_en := to_tsvector('english', unaccent(NEW.text));
  END IF;
  RETURN NEW;
END
$$ LANGUAGE plpgsql;

-- Sites
CREATE TABLE IF NOT EXISTS sites (
  id           bigserial PRIMARY KEY,
  domain       text NOT NULL UNIQUE,
  enabled      boolean NOT NULL DEFAULT true,
  rps_limit    integer NOT NULL DEFAULT 10,
  rps_burst    integer NOT NULL DEFAULT 20,
  depth_limit  integer NOT NULL DEFAULT 2,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_sites_updated_at
  BEFORE UPDATE ON sites
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Seeds for each site
CREATE TABLE IF NOT EXISTS site_seeds (
  id         bigserial PRIMARY KEY,
  site_id    bigint NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  url        text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (site_id, url)
);

-- Crawl queue without external broker
CREATE TABLE IF NOT EXISTS crawl_queue (
  id          bigserial PRIMARY KEY,
  site_id     bigint NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  url         text NOT NULL,
  url_hash    char(64) NOT NULL, -- sha256 hex, computed in application
  priority    integer NOT NULL DEFAULT 0,
  status      crawl_status NOT NULL DEFAULT 'queued',
  attempts    integer NOT NULL DEFAULT 0,
  last_error  text,
  next_try_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_crawl_queue_updated_at
  BEFORE UPDATE ON crawl_queue
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Only one active (queued/processing) task per (site,url_hash)
CREATE UNIQUE INDEX IF NOT EXISTS crawl_queue_site_urlhash_active_uq
  ON crawl_queue(site_id, url_hash)
  WHERE status IN ('queued','processing');

-- Picker-friendly index
CREATE INDEX IF NOT EXISTS crawl_queue_pick_idx
  ON crawl_queue(site_id, status, next_try_at, priority DESC, id);

-- Pages storage (stores original HTML for viewing and extracted text for search)
CREATE TABLE IF NOT EXISTS pages (
  id            bigserial PRIMARY KEY,
  site_id       bigint NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  url           text NOT NULL,
  url_hash      char(64) NOT NULL, -- sha256 hex
  title         text,
  description   text,
  lang          text CHECK (lang IN ('ru','en')),
  http_status   integer,
  content_type  text,              -- expected 'text/html; charset=...'
  headers       jsonb,             -- raw response headers (optional)
  charset       text,              -- detected charset on fetch
  raw_size      integer,           -- bytes of received body
  html_hash     char(64),          -- sha256 of original HTML (UTF-8 normalized)
  html          text,              -- original HTML (stored as UTF-8, for UI rendering)
  fetched_at    timestamptz,
  text          text,              -- extracted visible text for FTS
  tsv_ru        tsvector,
  tsv_en        tsvector,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (site_id, url_hash),
  CHECK (content_type IS NULL OR content_type ILIKE 'text/html%')
);

CREATE TRIGGER trg_pages_updated_at
  BEFORE UPDATE ON pages
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_pages_set_tsvectors
  BEFORE INSERT OR UPDATE OF text ON pages
  FOR EACH ROW EXECUTE FUNCTION pages_set_tsvectors();

-- FTS indexes
CREATE INDEX IF NOT EXISTS pages_tsv_ru_gin ON pages USING GIN (tsv_ru);
CREATE INDEX IF NOT EXISTS pages_tsv_en_gin ON pages USING GIN (tsv_en);
CREATE INDEX IF NOT EXISTS pages_site_fetched_idx ON pages(site_id, fetched_at DESC);

-- Links extracted from pages
CREATE TABLE IF NOT EXISTS page_links (
  id           bigserial PRIMARY KEY,
  from_page_id bigint NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  to_url       text NOT NULL,
  to_url_hash  char(64) NOT NULL, -- sha256 hex
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (from_page_id, to_url_hash)
);

-- Robots.txt cache per site
CREATE TABLE IF NOT EXISTS robots_cache (
  site_id     bigint PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
  robots_txt  text,
  fetched_at  timestamptz NOT NULL DEFAULT now(),
  ttl_until   timestamptz
);

-- Sitemaps per site
CREATE TABLE IF NOT EXISTS sitemaps (
  id         bigserial PRIMARY KEY,
  site_id    bigint NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  url        text NOT NULL,
  fetched_at timestamptz,
  ttl_until  timestamptz,
  UNIQUE (site_id, url)
);

-- Optional helper view for search union (logic is handled in application)
-- CREATE VIEW search_pages AS
-- SELECT id, site_id, url, title, description, fetched_at, tsv_ru, tsv_en
-- FROM pages;