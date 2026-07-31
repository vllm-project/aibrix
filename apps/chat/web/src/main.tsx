import { createRoot } from 'react-dom/client'
import App from './app/App'
import './styles/index.css'

// biome-ignore lint/style/noNonNullAssertion: root element always exists in index.html
createRoot(document.getElementById('root')!).render(<App />)
