@_silgen_name("compute_total")
func compute_total(_ items: UnsafePointer<Int32>, _ n: Int32) -> Int32

struct CheckoutService {
    func checkout(_ items: [Int32]) -> Int32 {
        return items.withUnsafeBufferPointer { buf in
            compute_total(buf.baseAddress!, Int32(buf.count))
        }
    }
}

let total = CheckoutService().checkout([1, 2, -3])
print(total)
