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
