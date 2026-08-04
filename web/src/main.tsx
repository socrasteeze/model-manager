import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { EnrichJobProvider } from './hooks/useEnrichJob'
import './styles.css'

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
