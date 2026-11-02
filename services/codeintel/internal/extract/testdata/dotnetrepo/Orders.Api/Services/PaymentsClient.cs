using Stripe;

namespace Orders.Api.Services;

public class PaymentsClient
{
    private readonly HttpClient _inventory = new() { BaseAddress = new Uri("https://inventory.internal.example.com/") };
    private const string Docs = "https://docs.example.org/not-a-call";

    public async Task<string> Ping(HttpClient http)
    {
        var verbatim = @"C:\temp\""quoted"" https://in-verbatim.example.com";
        return await http.GetStringAsync("https://status.example.net/health");
    }
}
