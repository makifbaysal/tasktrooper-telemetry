export default function handler(req, res) {
  if (req.method === 'DELETE') { return res.status(204).end(); }
  if (req.method === 'GET') { return res.json({}); }
}
