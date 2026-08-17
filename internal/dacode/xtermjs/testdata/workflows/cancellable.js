export const meta = {
  name: 'browser-cancellable-sweep',
  description: 'Hold a workflow agent call until cancellation',
  phases: [{title: 'Inspect', detail: 'One slow browser fixture agent'}],
}

phase('Inspect')
const result = await agent('Wait for the browser fixture response.', {
  label: 'browser-check',
  phase: 'Inspect',
})
return {result}
