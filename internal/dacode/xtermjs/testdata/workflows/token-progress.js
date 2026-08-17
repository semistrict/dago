export const meta = {
  name: 'browser-token-progress',
  description: 'Expose prompt and streamed output token estimates while a workflow worker remains active',
  phases: [{title: 'Inspect', detail: 'One streaming browser fixture agent'}],
}

phase('Inspect')
const result = await agent('TOKEN_PROGRESS_WORKER: stream a response slowly enough to observe usage growth.', {
  label: 'token-progress-worker',
  phase: 'Inspect',
})
return {result}
