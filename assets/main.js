window.onerror = (message, source, lineno, colno, error) => {
  if (message === 'Script error.' && !source && !lineno) return true // iOS share sheet noise
  console.error(
    `Error: ${message}\n\n${error?.stack ?? `${source}:${lineno}:${colno}`}`,
  )
}

window.onunhandledrejection = ({ reason }) =>
  console.error(
    `Unhandled Promise Rejection: ${reason?.message ?? reason}\n\n${
      reason?.stack ?? '(no stack)'
    }`,
  )

// Passkeys: fetch options, run the browser ceremony, submit the result.
document.addEventListener('click', async ev => {
  const el = ev.target.closest?.('[data-pk-create],[data-pk-get]')
  if (!el) return
  ev.preventDefault()
  const form = el.closest('form')
  const alertEl = form.querySelector('[role=alert]')
  if (alertEl) alertEl.textContent = ''
  const create = 'pkCreate' in el.dataset
  try {
    const res = await fetch(create ? el.dataset.pkCreate : el.dataset.pkGet)
    if (!res.ok) throw new Error(`The request failed (${res.status}).`)
    const { publicKey } = await res.json()
    const credential = create
      ? await navigator.credentials.create({
        publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(publicKey),
      })
      : await navigator.credentials.get({
        publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(publicKey),
      })
    form.json.value = JSON.stringify(credential)
    form.submit()
  } catch (err) {
    if (err.name === 'NotAllowedError' || err.name === 'AbortError') return
    console.error(err)
    if (alertEl) alertEl.textContent = err.message || 'Something went wrong.'
  }
})

// Copyable link fields: select the whole value on focus or click.
for (const type of ['focusin', 'click']) {
  document.addEventListener(type, ev => {
    const el = ev.target.closest?.('input[data-copy]')
    if (el) el.select()
  })
}

// Time zone: keep the tz cookie in step with the browser, so the server
// renders days and times in the viewer's zone. Reload once when it changed.
{
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
  const has = () => document.cookie.split('; ').includes(`tz=${tz}`)
  if (tz && !has()) {
    document.cookie = `tz=${tz}; path=/; max-age=31536000; samesite=lax`
    if (has()) location.reload()
  }
}

// Composer: Enter sends on devices with a fine pointer, Shift+Enter is a
// new line.
document.addEventListener('keydown', ev => {
  if (ev.target.id !== 'composer-body') return
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing) return
  if (!matchMedia('(pointer: fine)').matches) return
  ev.preventDefault()
  ev.target.form.requestSubmit()
})

// Live updates. refresh() fetches the current URL as a full page and morphs
// #app towards it. Only one runs at a time, and a request that arrives while
// one is running is never dropped.
let refreshing = false
let refreshAgain = false
async function refresh() {
  if (refreshing) {
    refreshAgain = true
    return
  }
  refreshing = true
  try {
    const res = await fetch(location.href, { headers: { 'X-Refresh': '1' } })
    if (res.redirected && new URL(res.url).pathname !== location.pathname) {
      location.href = res.url // signed out, or moved
      return
    }
    if (res.status === 404) {
      location.href = '/' // lost access
      return
    }
    if (!res.ok) return
    const doc = new DOMParser().parseFromString(await res.text(), 'text/html')
    const next = doc.getElementById('app')
    if (!next) return
    morph(document.getElementById('app'), next.outerHTML)
    document.title = doc.title
  } catch (err) {
    console.error(err) // offline: the next ping retries
  } finally {
    refreshing = false
    if (refreshAgain) {
      refreshAgain = false
      refresh()
    }
  }
}

// The event stream only says "something changed". It is closed while the
// tab is hidden, so an open stream means someone is looking.
if (document.getElementById('app')?.hasAttribute('data-live')) {
  let es = null
  let opened = false
  const connect = () => {
    if (es) return
    const chat = document.getElementById('app').dataset.chat
    es = new EventSource('/events' + (chat ? `?chat=${chat}` : ''))
    es.onmessage = refresh
    es.onopen = () => {
      if (opened) refresh() // catch up after a reconnect
      opened = true
    }
  }
  const disconnect = () => {
    es?.close()
    es = null
  }
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      disconnect()
    } else {
      refresh()
      connect()
    }
  })
  addEventListener('pagehide', disconnect)
  addEventListener('pageshow', ev => {
    if (ev.persisted && !document.hidden) {
      refresh()
      connect()
    }
  })
  if (!document.hidden) connect()
}

// data-morph forms post in the background and then refresh in place.
document.addEventListener('submit', async ev => {
  const form = ev.target
  if (!form.matches?.('form[data-morph]') || !window.morph) return
  ev.preventDefault()
  const button = ev.submitter
  const body = new URLSearchParams(new FormData(form, button))
  if (button) button.disabled = true
  try {
    const res = await fetch(form.action, { method: 'POST', body, redirect: 'manual' })
    if (res.type !== 'opaqueredirect' && !res.ok) throw new Error(`status ${res.status}`)
    form.reset()
    await refresh()
  } catch (err) {
    console.error(err)
    HTMLFormElement.prototype.submit.call(form) // let the server show its error page
    return
  } finally {
    if (button) button.disabled = false
  }
  if (form.id === 'composer') form.querySelector('textarea')?.focus()
})

// data-morph links change the URL and refresh in place.
document.addEventListener('click', ev => {
  const link = ev.target.closest?.('a[data-morph]')
  if (!link || !window.morph) return
  if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return
  ev.preventDefault()
  const url = new URL(link.href)
  history.replaceState(null, '', url.pathname + url.search)
  // Hold the reading position: #scroll is column-reverse, so its scrollTop
  // is the (negative) distance from the newest message. Browsers do not
  // keep it by themselves when the view is at the very top.
  const scroll = document.getElementById('scroll')
  const top = scroll?.scrollTop
  refresh().then(() => {
    if (scroll) scroll.scrollTop = top
  })
})
