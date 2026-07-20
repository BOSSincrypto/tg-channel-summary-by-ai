const CACHE_NAME = "digest-control-offline-v1";
const OFFLINE_URL = "./offline.html";
const INLINE_FALLBACK = "<!doctype html><html lang=\"ru\"><meta charset=\"utf-8\"><title>Digest Control</title><main role=\"alert\"><h1>Не удалось загрузить приложение</h1><p>Сервер настроек временно недоступен. Проверьте соединение и повторите попытку.</p><button onclick=\"location.reload()\">Повторить</button></main>";

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => cache.add(OFFLINE_URL))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((key) => key !== CACHE_NAME).map((key) => caches.delete(key))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  if (event.request.mode !== "navigate") return;
  event.respondWith(
    fetch(event.request).catch(() =>
      caches.match(OFFLINE_URL).then((response) => {
        if (!response) {
          return new Response(INLINE_FALLBACK, {
            headers: { "Content-Type": "text/html; charset=utf-8" },
            status: 503,
            statusText: "Service Unavailable"
          });
        }
        return response.text().then((body) => new Response(body, {
          headers: { "Content-Type": "text/html; charset=utf-8" },
          status: 503,
          statusText: "Service Unavailable"
        }));
      })
    )
  );
});
