// The service worker shows push notifications and opens the app when one is
// clicked. It does no caching and has no fetch handler.

addEventListener('install', () => skipWaiting())

// This worker keeps no caches, so any cache found was left by the reverted
// PWA worker that used to live at this URL. Delete them, and reload the open
// pages it served, since they are still running the old app.
addEventListener('activate', ev => ev.waitUntil((async () => {
  const names = await caches.keys()
  await Promise.all(names.map(name => caches.delete(name)))
  await clients.claim()
  if (names.length === 0) return
  const windows = await clients.matchAll({ type: 'window' })
  await Promise.all(windows.map(c => c.navigate(c.url).catch(console.error)))
})()))

addEventListener('push', ev => {
  let data = {}
  try {
    data = ev.data?.json() ?? {}
  } catch (err) {
    console.error(err)
  }
  // Browsers require a visible notification for every push.
  const { title = 'New message', body = '', tag, data: extra } = data
  ev.waitUntil(registration.showNotification(title, { body, tag, data: extra }))
})

addEventListener('notificationclick', ev => {
  ev.notification.close()
  const url = new URL(ev.notification.data?.url ?? '/', location.origin).href
  ev.waitUntil(Promise.all([
    fetch('/push/opened', { method: 'POST', keepalive: true }).catch(console.error),
    (async () => {
      const windows = await clients.matchAll({ type: 'window' })
      const client = windows.find(c => new URL(c.url).origin === location.origin)
      if (!client) return clients.openWindow(url)
      await client.focus()
      return client.navigate(url)
    })(),
  ]))
})
