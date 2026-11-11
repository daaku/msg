// The service worker shows push notifications and opens the app when one is
// clicked. It does no caching and has no fetch handler.

addEventListener('install', () => skipWaiting())

addEventListener('activate', ev => ev.waitUntil(clients.claim()))

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
