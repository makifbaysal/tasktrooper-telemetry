import { createBrowserRouter, Route, Routes } from 'react-router-dom';
const router = createBrowserRouter([
  { path: '/', element: <Home /> },
  { path: '/orders/:orderId', element: <Order /> },
]);
export function App() {
  return (
    <Routes>
      <Route path="/settings" element={<Settings />} />
      <Route path="/checkout" element={<Checkout />} />
    </Routes>
  );
}
