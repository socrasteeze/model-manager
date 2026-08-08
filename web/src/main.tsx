import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { EnrichJobProvider } from './hooks/useEnrichJob'
import { scrollFocusedFieldIntoView, trackKeyboardInset } from './keyboard'
import './styles.css'

// Outside React: these are document-level listeners with no component that owns
// them, and they must be live before the first field can be focused.
trackKeyboardInset()
scrollFocusedFieldIntoView()

const container = document.getElementById('root')
if (container) {
  createRoot(container).render(
    <StrictMode>
      <EnrichJobProvider>
        <App />
      </EnrichJobProvider>
    </StrictMode>,
  )
}
