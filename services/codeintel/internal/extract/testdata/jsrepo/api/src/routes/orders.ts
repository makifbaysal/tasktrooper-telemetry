import { Router } from 'express';
const router = Router();

router.get('/:id', async (req, res) => {
  res.json({});
});
router.delete('/:id', cancelOrder);

export async function cancelOrder(req, res) {
  // refund + cancel
}
export default router;
