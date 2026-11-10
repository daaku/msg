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
