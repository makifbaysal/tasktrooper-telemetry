import express from 'express';
import { init } from '@tasktrooper/node';
import ordersRouter from './routes/orders';
import axios from 'axios';
import { Pool } from 'pg';

init({ dsn: process.env.TROOPER_DSN, service: 'checkout-api' });

const app = express();
app.use(express.json());
app.use('/v1/orders', ordersRouter);

// app.get('/commented-out', handler)
/* app.post('/also-commented', handler) */
app.get('/healthz', (req, res) => res.send('ok'));
app.post('/v1/checkout', authenticate, createCheckout);
app.route('/v1/cart/:cartId').get(getCart).put(updateCart);

const msg = "app.get('/inside-string', x)";

async function createCheckout(req, res) {
  const r = await axios.post('https://api.stripe.com/v1/payment_intents', {});
  res.json(r.data);
}
function getCart(req, res) {}
function updateCart(req, res) {}
function authenticate(req, res, next) { next(); }

const client = axios.create();
client.get('/v1/not-a-route', {});
