import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { useAuth } from './lib/auth';
import Nav from './components/Nav';
import Login from './pages/Login';
import Home from './pages/Home';
import Search from './pages/Search';
import Detail from './pages/Detail';
import My from './pages/My';
import Sources from './pages/Sources';

export default function App() {
  const { ready, verified } = useAuth();
  const loc = useLocation();

  if (!ready) {
    return <div className="center-fill"><div className="spinner" /></div>;
  }
  if (!verified) {
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="*" element={<Navigate to="/login" replace state={{ from: loc.pathname + loc.search }} />} />
      </Routes>
    );
  }

  return (
    <>
      <Nav />
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/search" element={<Search />} />
        <Route path="/play/:sourceKey/:vodId" element={<Detail />} />
        <Route path="/me" element={<My />} />
        <Route path="/sources" element={<Sources />} />
        <Route path="/login" element={<Navigate to="/" replace />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </>
  );
}
