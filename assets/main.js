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

// Push: register the service worker for signed in users, mark <body> with
// the notification permission (push-granted, push-denied, push-prompt or
// push-unavailable), and subscribe this device when asked.
{
  const sw = document.querySelector('meta[data-sw]')?.dataset.sw
  const mark = state => {
    document.body.classList.remove('push-granted', 'push-denied', 'push-prompt', 'push-unavailable')
    document.body.classList.add(`push-${state}`)
  }
  if (sw) {
    ;(async () => {
      try {
        const registration = await navigator.serviceWorker.register(sw, { updateViaCache: 'none' })
        mark(await registration.pushManager?.permissionState({ userVisibleOnly: true }) ?? 'unavailable')
      } catch (err) {
        console.error(err)
        mark('unavailable')
      }
    })()
  }

  document.addEventListener('click', async ev => {
    const el = ev.target.closest?.('[data-push-subscribe]')
    if (!el) return
    ev.preventDefault()
    const form = el.closest('form')
    const alertEl = form.querySelector('[role=alert]')
    if (alertEl) alertEl.textContent = ''
    try {
      const registration = await navigator.serviceWorker.ready
      const options = {
        userVisibleOnly: true,
        applicationServerKey: Uint8Array.fromBase64(el.dataset.pushSubscribe, { alphabet: 'base64url' }),
      }
      let subscription
      try {
        subscription = await registration.pushManager.subscribe(options)
      } catch (err) {
        if (err.name !== 'InvalidStateError' && !err.message?.includes('application server key')) throw err
        // A subscription made with a different key is in the way: drop it
        // and try again.
        await (await registration.pushManager.getSubscription())?.unsubscribe()
        subscription = await registration.pushManager.subscribe(options)
      }
      form.json.value = JSON.stringify(subscription)
      form.submit()
    } catch (err) {
      if (err.name === 'NotAllowedError') {
        mark('denied')
        return
      }
      console.error(err)
      if (alertEl) alertEl.textContent = err.message || 'Something went wrong.'
    }
  })
}

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
let refreshHeld = false
async function refresh() {
  // The crop dialog holds state the morph would wipe (the canvas, the place
  // of the selection, the dialog being open), so wait until it has closed.
  if (document.querySelector('dialog.crop[open]')) {
    refreshHeld = true
    return
  }
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
    showUploadError()
    showCurrentThumb()
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

// The media page: bring the thumbnail of the shown item into the middle of
// the strip, once each time another item is shown.
let shownThumb = ''
function showCurrentThumb() {
  const thumb = document.querySelector('.gallery-thumb[aria-current]')
  if (!thumb || thumb.id === shownThumb) return
  shownThumb = thumb.id
  const strip = thumb.parentElement
  strip.scrollLeft = thumb.offsetLeft - (strip.clientWidth - thumb.offsetWidth) / 2
}
showCurrentThumb()

// A chat opened at a message (#m123) forgets that once the reader scrolls
// away or sends something: the fragment is dropped from the URL in place,
// with no history entry and no reload, so a later reload does not jump back.
{
  const dropHash = () => {
    if (location.hash) history.replaceState(history.state, '', location.pathname + location.search)
  }
  let base = null // where the browser's own jump to the fragment left #scroll
  addEventListener('load', () =>
    requestAnimationFrame(() => {
      base = document.getElementById('scroll')?.scrollTop ?? null
    }),
  )
  // scroll does not bubble, so it is caught on the way down
  document.addEventListener(
    'scroll',
    ev => {
      if (ev.target.id !== 'scroll' || base === null || !location.hash) return
      if (Math.abs(ev.target.scrollTop - base) > 32) dropHash()
    },
    { capture: true, passive: true },
  )
  document.addEventListener('submit', ev => {
    if (ev.target.id === 'composer') dropHash()
  })
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
    form.closest('[popover]')?.hidePopover()
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

// Uploads: chosen files go straight to S3, one at a time, one part at a
// time, in the background. The ring is drawn by CSS from --upload and the
// uploading class on <html>, which the morph never touches. The error text
// lives inside #app, so it is kept here and put back after every refresh.
let uploadError = ''
function showUploadError() {
  const el = document.getElementById('upload-error')
  if (el && el.textContent !== uploadError) el.textContent = uploadError
}
{
  const root = document.documentElement
  const queue = []
  let total = 0 // bytes queued since the queue was last empty
  let sent = 0 // bytes of finished parts and finished or failed files
  const progress = loaded => {
    const pct = total ? Math.min(100, (100 * (sent + loaded)) / total) : 0
    root.style.setProperty('--upload', pct)
    document.getElementById('upload-ring')?.setAttribute('aria-valuenow', Math.round(pct))
  }
  const addError = msg => {
    uploadError = uploadError ? `${uploadError}\n${msg}` : msg
    showUploadError()
  }
  const confirmLeave = ev => {
    ev.preventDefault()
    ev.returnValue = ''
  }
  const fmtBytes = n => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB']
    let i = 0
    while (n >= 1024 && i < units.length - 1) {
      n /= 1024
      i++
    }
    return `${+n.toFixed(1)} ${units[i]}`
  }
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
  const online = () => new Promise(resolve => addEventListener('online', resolve, { once: true }))

  const api = async (path, body) => {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    const out = await res.json().catch(() => ({}))
    if (!res.ok) throw new Error(out.error || `status ${res.status}`)
    return out
  }

  // XMLHttpRequest, because fetch cannot report upload progress.
  const put = (url, blob) =>
    new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.open('PUT', url)
      xhr.upload.onprogress = ev => progress(ev.loaded)
      xhr.onload = () =>
        xhr.status >= 200 && xhr.status < 300
          ? resolve()
          : reject(new Error(`PUT status ${xhr.status}`))
      xhr.onerror = xhr.onabort = xhr.ontimeout = () => reject(new Error('PUT failed'))
      xhr.send(blob)
    })

  const putRetry = async (url, blob) => {
    for (let attempt = 0; ; attempt++) {
      try {
        return await put(url, blob)
      } catch (err) {
        if (attempt === 5) throw err
        progress(0)
        await sleep(1000 << attempt) // 1, 2, 4, 8, 16 seconds
        while (!navigator.onLine) await online()
      }
    }
  }

  const upload = async ({ file, chat }) => {
    const start = sent
    let id = 0
    try {
      const init = await api('/upload/init', { chat, name: file.name, size: file.size })
      id = init.file
      for (const [i, url] of init.urls.entries()) {
        const part = file.slice(i * init.partSize, (i + 1) * init.partSize)
        await putRetry(url, part)
        sent += part.size
        progress(0)
      }
      await api('/upload/complete', { file: id })
    } catch (err) {
      console.error(err)
      if (id) api('/upload/abort', { file: id }).catch(console.error)
      addError(`${file.name} could not be uploaded`)
    }
    sent = start + file.size
    progress(0)
  }

  let running = false
  const work = async () => {
    running = true
    root.classList.add('uploading')
    addEventListener('beforeunload', confirmLeave)
    while (queue.length) {
      await upload(queue[0])
      queue.shift() // only now, so the queue is not empty while a file is in flight
    }
    removeEventListener('beforeunload', confirmLeave)
    root.classList.remove('uploading')
    total = sent = 0
    progress(0)
    running = false
  }

  document.addEventListener('change', ev => {
    const input = ev.target.closest?.('input[data-upload]')
    if (!input) return
    const files = [...input.files]
    input.value = '' // so the same file can be chosen again
    const chat = Number(input.dataset.upload)
    const max = Number(input.dataset.max)
    uploadError = ''
    showUploadError()
    for (const file of files) {
      if (file.size > max) {
        addError(`${file.name} is larger than the ${fmtBytes(max)} limit`)
        continue
      }
      queue.push({ file, chat })
      total += file.size
    }
    progress(0)
    if (queue.length && !running) work()
  })
}

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

// Profile picture and group icon: a chosen image opens in the crop dialog, where a square
// selection is moved and resized over it. Saving posts only that square,
// scaled down, as a JPEG data URL in an ordinary form. The image is drawn on
// a canvas, which the CSP allows where it would refuse a blob: <img>.
{
  const OUT = 512 // largest edge posted; avatarSize in auth.go
  const SOURCE = 2048 // long edge of the working canvas
  const SMALLEST = 56 // smallest selection, in CSS pixels
  const $ = id => document.getElementById(id)
  const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi)
  let sel = null // { x, y, size } in canvas pixels, while the dialog is open
  let drag = null

  // The box is placed in percent of the stage, so it follows a resize.
  const place = () => {
    const { width, height } = $('crop-canvas')
    const s = $('crop-box').style
    s.left = `${(100 * sel.x) / width}%`
    s.top = `${(100 * sel.y) / height}%`
    s.width = `${(100 * sel.size) / width}%`
    s.height = `${(100 * sel.size) / height}%`
  }
  // geometry gives the canvas size, its place on screen, and how many canvas
  // pixels one CSS pixel covers.
  const geometry = () => {
    const canvas = $('crop-canvas')
    const rect = canvas.getBoundingClientRect()
    const density = canvas.width / rect.width
    return {
      width: canvas.width,
      height: canvas.height,
      rect,
      density,
      smallest: Math.min(canvas.width, canvas.height, SMALLEST * density),
    }
  }
  const moveTo = (x, y) => {
    const { width, height } = $('crop-canvas')
    sel = { size: sel.size, x: clamp(x, 0, width - sel.size), y: clamp(y, 0, height - sel.size) }
  }

  document.addEventListener('change', async ev => {
    const input = ev.target.closest?.('input[data-avatar]')
    if (!input) return
    const file = input.files[0]
    input.value = '' // so the same file can be chosen again
    if (!file) return
    const error = $('avatar-error')
    error.textContent = ''
    let bitmap
    try {
      bitmap = await createImageBitmap(file)
    } catch (err) {
      console.error(err)
      error.textContent = 'That file could not be opened as a picture.'
      return
    }
    const canvas = $('crop-canvas')
    const scale = Math.min(1, SOURCE / Math.max(bitmap.width, bitmap.height))
    const width = (canvas.width = Math.max(1, Math.round(bitmap.width * scale)))
    const height = (canvas.height = Math.max(1, Math.round(bitmap.height * scale)))
    const ctx = canvas.getContext('2d')
    ctx.fillStyle = '#fff' // behind a transparent image: JPEG has no alpha
    ctx.fillRect(0, 0, width, height)
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage(bitmap, 0, 0, width, height)
    bitmap.close()
    canvas.parentElement.style.setProperty('--ratio', width / height)
    const size = Math.min(width, height) // the largest square, centred
    sel = { size, x: (width - size) / 2, y: (height - size) / 2 }
    place()
    $(input.dataset.avatar).showModal()
    $('crop-box').focus()
  })

  document.addEventListener('pointerdown', ev => {
    const box = ev.target.closest?.('#crop-box')
    if (!box || !sel || ev.button !== 0) return
    ev.preventDefault()
    box.setPointerCapture(ev.pointerId)
    box.focus({ preventScroll: true })
    const { rect, density } = geometry()
    const px = (ev.clientX - rect.left) * density
    const py = (ev.clientY - rect.top) * density
    drag = { id: ev.pointerId, from: sel, dx: sel.x - px, dy: sel.y - py }
    const corner = ev.target.dataset.corner // nw, ne, sw or se
    if (corner) {
      // The corner opposite to the one held stays where it is. grab is how
      // far the pointer is from the corner it holds.
      const sx = corner[1] === 'e' ? 1 : -1
      const sy = corner[0] === 's' ? 1 : -1
      const ax = sx > 0 ? sel.x : sel.x + sel.size
      const ay = sy > 0 ? sel.y : sel.y + sel.size
      drag.corner = { sx, sy, ax, ay, gx: ax + sx * sel.size - px, gy: ay + sy * sel.size - py }
    }
  })

  document.addEventListener('pointermove', ev => {
    if (!drag || ev.pointerId !== drag.id) return
    const { width, height, rect, density, smallest } = geometry()
    const px = (ev.clientX - rect.left) * density
    const py = (ev.clientY - rect.top) * density
    if (drag.corner) {
      const { sx, sy, ax, ay, gx, gy } = drag.corner
      const room = Math.min(sx > 0 ? width - ax : ax, sy > 0 ? height - ay : ay)
      const want = Math.max((px + gx - ax) * sx, (py + gy - ay) * sy)
      const size = clamp(want, Math.min(smallest, room), room)
      sel = { size, x: sx > 0 ? ax : ax - size, y: sy > 0 ? ay : ay - size }
    } else {
      moveTo(px + drag.dx, py + drag.dy)
    }
    place()
  })

  for (const type of ['pointerup', 'pointercancel']) {
    document.addEventListener(type, ev => {
      if (drag?.id === ev.pointerId) drag = null
    })
  }

  // Arrow keys move the selection, plus and minus resize it about its centre.
  document.addEventListener('keydown', ev => {
    if (ev.target.id !== 'crop-box' || !sel) return
    const { width, height, smallest } = geometry()
    const step = Math.max(1, Math.round(Math.min(width, height) / (ev.shiftKey ? 10 : 50)))
    const move = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] }[ev.key]
    const grow = { '+': 1, '=': 1, '-': -1, _: -1 }[ev.key]
    if (move) {
      moveTo(sel.x + move[0] * step, sel.y + move[1] * step)
    } else if (grow) {
      const size = clamp(sel.size + 2 * grow * step, smallest, Math.min(width, height))
      const shift = (sel.size - size) / 2
      sel.size = size
      moveTo(sel.x + shift, sel.y + shift)
    } else {
      return
    }
    ev.preventDefault()
    place()
  })

  document.addEventListener('submit', ev => {
    const form = ev.target
    if (!form.closest?.('#crop') || !sel || ev.submitter?.formMethod === 'dialog') return
    const side = Math.min(OUT, Math.round(sel.size))
    const out = document.createElement('canvas')
    out.width = out.height = side
    const ctx = out.getContext('2d')
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage($('crop-canvas'), sel.x, sel.y, sel.size, sel.size, 0, 0, side, side)
    form.image.value = out.toDataURL('image/jpeg', 0.9)
  })

  // close does not bubble, so it is caught on the way down.
  document.addEventListener(
    'close',
    ev => {
      if (ev.target.id !== 'crop') return
      sel = drag = null
      const canvas = $('crop-canvas')
      canvas.width = canvas.height = 0 // free the pixels
      if (refreshHeld) {
        refreshHeld = false
        refresh()
      }
    },
    true,
  )
}
