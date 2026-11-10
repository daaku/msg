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
